import { spawn } from "node:child_process";
import type { ChildProcess } from "node:child_process";
import type { ExitRecord, StopReason, StopRecord } from "./contract.js";
import { journalMaxBytes, stopGraceMs } from "./contract.js";
import { writeExit, writeReady, readStop } from "./execution-state.js";
import { JournalWriter } from "./journal.js";
import { journalPath } from "./execution-state.js";

export interface SupervisorOptions {
  stateRoot: string;
  executionId: string;
  nonce: string;
  /** Task command (normally the node binary that re-enters the CLI). */
  taskCommand: string;
  /** Task arguments (the subcommand plus its flags). */
  taskArgs: string[];
  /** Overrides for tests. */
  maxBytes?: number;
  graceMs?: number;
  pollMs?: number;
  /** Environment additions for the task. */
  extraEnv?: Record<string, string>;
}

interface CloseOutcome {
  code: number | null;
  signal: NodeJS.Signals | null;
}

/**
 * Run the detached task to completion and maintain the execution state files.
 *
 * The supervisor is the task's parent for the whole run: it spawns the task in
 * its own process group (`detached`), redirects the task's stdout/stderr into
 * the journal, publishes readiness, forwards a stop request when one arrives,
 * and finally writes the atomic exit record.
 */
export async function runSupervisor(options: SupervisorOptions): Promise<void> {
  const { stateRoot, executionId, nonce } = options;
  const maxBytes = options.maxBytes ?? journalMaxBytes();
  const graceMs = options.graceMs ?? stopGraceMs();
  const pollMs = options.pollMs ?? 50;

  const task = spawn(options.taskCommand, options.taskArgs, {
    detached: true,
    stdio: ["ignore", "pipe", "pipe"],
    env: { ...process.env, AGENT_COMPOSE_EXECUTION_NONCE: nonce, ...options.extraEnv },
  });

  const closePromise = waitForClose(task);
  const journal = new JournalWriter(journalPath(stateRoot, executionId), maxBytes);
  task.stdout?.on("data", (chunk: Buffer) => {
    void journal.append("stdout", chunk.toString("utf8"));
  });
  task.stderr?.on("data", (chunk: Buffer) => {
    void journal.append("stderr", chunk.toString("utf8"));
  });

  await writeReady(stateRoot, executionId, { pid: process.pid, nonce, taskPid: task.pid ?? 0 });

  let stopRecord: StopRecord | null = null;
  let outcome: CloseOutcome | null = null;
  while (stopRecord === null && outcome === null) {
    const raced = await Promise.race([
      closePromise.then((closed) => ({ closed }) as const),
      sleep(pollMs).then(() => ({ closed: null }) as const),
    ]);
    if (raced.closed) {
      outcome = raced.closed;
      break;
    }
    stopRecord = await readStop(stateRoot, executionId);
  }

  if (stopRecord !== null) {
    await terminateTask(task, graceMs);
    outcome = await waitForClose(task);
  }

  await journal.flush();
  const record = buildExitRecord(outcome ?? { code: null, signal: null }, stopRecord);
  await writeExit(stateRoot, executionId, record);
}

async function terminateTask(task: ChildProcess, graceMs: number): Promise<void> {
  if (task.exitCode !== null || task.signalCode !== null) {
    return;
  }
  signalGroup(task, "SIGTERM");
  const closed = await Promise.race([
    waitForClose(task).then(() => true),
    sleep(graceMs).then(() => false),
  ]);
  if (!closed) {
    signalGroup(task, "SIGKILL");
    await waitForClose(task);
  }
}

function signalGroup(task: ChildProcess, signal: NodeJS.Signals): void {
  const pid = task.pid;
  if (!pid) {
    return;
  }
  try {
    process.kill(-pid, signal);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code !== "ESRCH") {
      // Delivery can race with process-group exit; the close event is the
      // source of truth.
    }
  }
}

function waitForClose(task: ChildProcess): Promise<CloseOutcome> {
  return new Promise((resolve) => {
    if (task.exitCode !== null || task.signalCode !== null) {
      resolve({ code: task.exitCode, signal: task.signalCode });
      return;
    }
    task.once("close", (code, signal) => resolve({ code, signal }));
  });
}

function buildExitRecord(outcome: CloseOutcome, stopRecord: StopRecord | null): ExitRecord {
  if (stopRecord !== null) {
    const reason: StopReason = stopRecord.reason;
    return {
      code: normalizeCode(outcome),
      reason: "stopped",
      stopReason: reason,
      ...(outcome.signal ? { signal: outcome.signal } : {}),
    };
  }
  if (outcome.signal) {
    return { code: 128 + signalNumber(outcome.signal), reason: "signal", signal: outcome.signal };
  }
  return { code: outcome.code ?? 0, reason: "completed" };
}

function normalizeCode(outcome: CloseOutcome): number {
  if (outcome.signal) {
    return 128 + signalNumber(outcome.signal);
  }
  return outcome.code ?? 0;
}

function signalNumber(signal: string): number {
  const table: Record<string, number> = {
    SIGHUP: 1,
    SIGINT: 2,
    SIGQUIT: 3,
    SIGKILL: 9,
    SIGSEGV: 11,
    SIGPIPE: 13,
    SIGALRM: 14,
    SIGTERM: 15,
  };
  return table[signal] ?? 128;
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
