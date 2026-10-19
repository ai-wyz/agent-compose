import fs from "node:fs/promises";
import path from "node:path";
import type { ExitRecord, ReadyRecord, StopRecord } from "./contract.js";

export function executionDir(stateRoot: string, executionId: string): string {
  return path.join(stateRoot, "executions", executionId);
}

export function journalPath(stateRoot: string, executionId: string): string {
  return path.join(executionDir(stateRoot, executionId), "journal");
}

export function readyPath(stateRoot: string, executionId: string): string {
  return path.join(executionDir(stateRoot, executionId), "ready.json");
}

export function exitPath(stateRoot: string, executionId: string): string {
  return path.join(executionDir(stateRoot, executionId), "exit.json");
}

export function stopPath(stateRoot: string, executionId: string): string {
  return path.join(executionDir(stateRoot, executionId), "stop.json");
}

/** Write a file atomically: temp file + rename so readers never see a partial write. */
export async function writeFileAtomic(filePath: string, content: string): Promise<void> {
  const dir = path.dirname(filePath);
  await fs.mkdir(dir, { recursive: true });
  const tmp = `${filePath}.${process.pid}.${Date.now()}.tmp`;
  await fs.writeFile(tmp, content, { encoding: "utf8", mode: 0o600 });
  await fs.rename(tmp, filePath);
}

export async function writeReady(stateRoot: string, executionId: string, record: ReadyRecord): Promise<void> {
  await writeFileAtomic(readyPath(stateRoot, executionId), `${JSON.stringify(record)}\n`);
}

export async function readReady(stateRoot: string, executionId: string): Promise<ReadyRecord | null> {
  try {
    const raw = await fs.readFile(readyPath(stateRoot, executionId), "utf8");
    const parsed = JSON.parse(raw) as Partial<ReadyRecord>;
    if (typeof parsed.pid !== "number" || typeof parsed.nonce !== "string" || typeof parsed.taskPid !== "number") {
      return null;
    }
    return { pid: parsed.pid, nonce: parsed.nonce, taskPid: parsed.taskPid };
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      return null;
    }
    throw error;
  }
}

export async function writeExit(stateRoot: string, executionId: string, record: ExitRecord): Promise<void> {
  await writeFileAtomic(exitPath(stateRoot, executionId), `${JSON.stringify(record)}\n`);
}

export async function readExit(stateRoot: string, executionId: string): Promise<ExitRecord | null> {
  try {
    const raw = await fs.readFile(exitPath(stateRoot, executionId), "utf8");
    return parseExit(raw);
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      return null;
    }
    throw error;
  }
}

export function parseExit(raw: string): ExitRecord | null {
  try {
    const parsed = JSON.parse(raw) as Partial<ExitRecord>;
    if (typeof parsed.code !== "number" || typeof parsed.reason !== "string") {
      return null;
    }
    return {
      code: parsed.code,
      reason: parsed.reason as ExitRecord["reason"],
      stopReason: parsed.stopReason as ExitRecord["stopReason"] | undefined,
      signal: parsed.signal,
    };
  } catch {
    return null;
  }
}

export async function writeStop(stateRoot: string, executionId: string, record: StopRecord): Promise<void> {
  await writeFileAtomic(stopPath(stateRoot, executionId), `${JSON.stringify(record)}\n`);
}

export async function readStop(stateRoot: string, executionId: string): Promise<StopRecord | null> {
  try {
    const raw = await fs.readFile(stopPath(stateRoot, executionId), "utf8");
    const parsed = JSON.parse(raw) as Partial<StopRecord>;
    if (parsed.reason !== "user" && parsed.reason !== "timeout") {
      return null;
    }
    return { reason: parsed.reason };
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      return null;
    }
    throw error;
  }
}

/** True when the process identified by pid exists. */
export function processAlive(pid: number): boolean {
  try {
    process.kill(pid, 0);
    return true;
  } catch (error) {
    return (error as NodeJS.ErrnoException).code === "EPERM";
  }
}
