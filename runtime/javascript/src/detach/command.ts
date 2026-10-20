import { spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import type { Writable } from "node:stream";
import { DETACH_POLL_INTERVAL_MS, DEFAULT_READY_TIMEOUT_MS, FINISHED_EXECUTION_RETENTION_MS, type AttachRecord, type Capabilities, type ExitRecord, type JournalRecord, type StopReason } from "./contract.js";
import { journalMaxBytes, stopGraceMs } from "./contract.js";
import fs from "node:fs/promises";
import {
  cleanupExecution,
  executionDir,
  journalPath,
  pruneFinishedExecutions,
  readExit,
  readReady,
  supervisorAlive,
  writeStop,
} from "./execution-state.js";
import { JournalWriter, readJournalFrom } from "./journal.js";
import { runSupervisor } from "./supervisor.js";

export function capabilities(): Capabilities {
  return { detach: true };
}

export function resolveCliPath(argv1: string = process.argv[1]): string {
  return argv1;
}

export interface StartDetachedOptions {
  stateRoot: string;
  executionId: string;
  /** The subcommand and flags to run, e.g. ["prompt", "--provider", "codex", ...]. */
  taskArgs: string[];
  cliPath?: string;
  /** How long to wait for the supervisor's readiness file. */
  timeoutMs?: number;
  /** Run timeout the supervisor enforces on its own; 0 or absent disables it. */
  taskTimeoutMs?: number;
  pollMs?: number;
}

/**
 * Start a detached execution: spawn the supervisor in its own session and
 * return as soon as the supervisor reports readiness. The task keeps running
 * after this exec ends.
 */
export async function startDetached(options: StartDetachedOptions): Promise<void> {
  // Best-effort: a failed prune must not block the run.
  await pruneFinishedExecutions(options.stateRoot, FINISHED_EXECUTION_RETENTION_MS).catch(() => undefined);
  const nonce = randomBytes(16).toString("hex");
  const cliPath = options.cliPath ?? resolveCliPath();
  const supervisorArgs = [
    "supervisor",
    "--execution-id",
    options.executionId,
    "--state-root",
    options.stateRoot,
    "--nonce",
    nonce,
    ...(options.taskTimeoutMs && options.taskTimeoutMs > 0 ? ["--task-timeout-ms", String(Math.floor(options.taskTimeoutMs))] : []),
    "--",
    ...options.taskArgs,
  ];
  const child = spawn(process.execPath, [cliPath, ...supervisorArgs], {
    detached: true,
    stdio: "ignore",
  });
  child.unref();
  await waitForReady(options.stateRoot, options.executionId, options.timeoutMs ?? DEFAULT_READY_TIMEOUT_MS, options.pollMs);
}

async function waitForReady(stateRoot: string, executionId: string, timeoutMs: number, pollMs?: number): Promise<void> {
  const interval = pollMs ?? DETACH_POLL_INTERVAL_MS;
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const ready = await readReady(stateRoot, executionId);
    if (ready) {
      return;
    }
    if (Date.now() >= deadline) {
      throw new Error(`detached execution ${executionId} did not become ready within ${timeoutMs}ms`);
    }
    await sleep(interval);
  }
}

export interface AttachExecutionOptions {
  stateRoot: string;
  executionId: string;
  from: number;
  stdout?: Writable;
  pollMs?: number;
}

/**
 * Replay the journal from `from` and follow it until the execution ends, then
 * emit the exit record and return the process exit code to use. Emits NDJSON
 * AttachRecord lines to `stdout`.
 */
export async function attachExecution(options: AttachExecutionOptions): Promise<number> {
  const stdout = options.stdout ?? process.stdout;
  const pollMs = options.pollMs ?? DETACH_POLL_INTERVAL_MS;

  let cursor = options.from;
  // Skip re-reading the journal while it is unchanged: appends grow it and
  // compaction replaces the file (new inode), so either changes the signature.
  let journalSignature = "";
  const drainJournal = async (): Promise<void> => {
    const signature = await statSignature(journalPath(options.stateRoot, options.executionId));
    if (signature !== "" && signature === journalSignature) {
      return;
    }
    journalSignature = signature;
    const records = await readJournalFrom(options.stateRoot, options.executionId, cursor);
    for (const record of records) {
      emit(stdout, journalToAttach(record));
      cursor = journalRecordEnd(record);
    }
  };
  // The supervisor flushes the journal before it writes the exit record, so
  // once the exit record is seen one more journal read is complete. Without
  // it, output flushed between the journal read and the exit check is lost.
  const finish = async (exit: ExitRecord): Promise<number> => {
    await drainJournal();
    emit(stdout, exitRecord(exit));
    return processExitCode(exit.code);
  };
  for (;;) {
    await drainJournal();
    const exit = await readExit(options.stateRoot, options.executionId);
    if (exit) {
      return finish(exit);
    }
    // No exit record yet: the task is only alive if the supervisor that owns
    // it is. A dead supervisor with no exit record means the task died
    // unmanaged (e.g. the supervisor itself was killed), so report it instead
    // of hanging forever.
    const ready = await readReady(options.stateRoot, options.executionId);
    if (!ready || !(await supervisorAlive(ready))) {
      // The supervisor may have written the exit record and exited right
      // after the check above; re-check before calling it abnormal.
      const lateExit = await readExit(options.stateRoot, options.executionId);
      if (lateExit) {
        return finish(lateExit);
      }
      await drainJournal();
      emit(stdout, { t: "exit", code: -1, reason: "abnormal" });
      return 1;
    }
    await sleep(pollMs);
  }
}

export interface StopExecutionOptions {
  stateRoot: string;
  executionId: string;
  reason: StopReason;
}

export async function stopExecution(options: StopExecutionOptions): Promise<void> {
  await writeStop(options.stateRoot, options.executionId, { reason: options.reason });
}

export interface CleanupExecutionOptions {
  stateRoot: string;
  executionId: string;
}

/** Remove a finished execution's state; see cleanupExecution. */
export async function cleanupDetached(options: CleanupExecutionOptions): Promise<void> {
  await cleanupExecution(options.stateRoot, options.executionId);
}

async function statSignature(filePath: string): Promise<string> {
  try {
    const stat = await fs.stat(filePath);
    return `${stat.ino}:${stat.size}:${stat.mtimeMs}`;
  } catch {
    return "";
  }
}

/** Paths and journal handles needed by the internal supervisor subcommand. */
export { executionDir, journalPath, JournalWriter, journalMaxBytes, runSupervisor, stopGraceMs };

function journalToAttach(record: JournalRecord): AttachRecord {
  if (record.kind === "out") {
    return { t: "out", s: record.stream, text: record.text, offset: record.offset };
  }
  return { t: "gap", from: record.from, to: record.to };
}

function exitRecord(record: { code: number; reason: string; stopReason?: StopReason; signal?: string }): Extract<AttachRecord, { t: "exit" }> {
  return {
    t: "exit",
    code: record.code,
    reason: record.reason as Extract<AttachRecord, { t: "exit" }>["reason"],
    ...(record.stopReason ? { stopReason: record.stopReason } : {}),
    ...(record.signal ? { signal: record.signal } : {}),
  };
}

function journalRecordEnd(record: JournalRecord): number {
  return record.kind === "out" ? record.offset + Buffer.byteLength(record.text, "utf8") : record.to;
}

function processExitCode(code: number): number {
  return code < 0 ? 1 : code;
}

function emit(stdout: Writable, record: AttachRecord): void {
  stdout.write(`${JSON.stringify(record)}\n`);
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
