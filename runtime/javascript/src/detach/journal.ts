import fs from "node:fs/promises";
import path from "node:path";
import type { JournalRecord, StdioStream } from "./contract.js";

/**
 * Append-only journal of the task's stdout/stderr plus gap markers, keyed by a
 * monotonic byte offset in the merged stream. When the file grows past
 * `maxBytes` the writer drops the oldest records and records the dropped byte
 * range as a gap, so a resuming reader can always tell what it missed.
 */
export class JournalWriter {
  private offset = 0;
  private size = 0;
  private chain: Promise<void> = Promise.resolve();

  constructor(
    private readonly filePath: string,
    private readonly maxBytes: number,
  ) {}

  /** Next byte offset an appended record will start at. */
  nextOffset(): number {
    return this.offset;
  }

  append(stream: StdioStream, text: string): Promise<void> {
    if (text.length === 0) {
      return this.chain;
    }
    const bytes = Buffer.byteLength(text, "utf8");
    const record: JournalRecord = { kind: "out", offset: this.offset, stream, text };
    this.offset += bytes;
    this.chain = this.chain.then(() => this.writeRecord(record, bytes));
    return this.chain;
  }

  /** Flush all pending appends and return once the file is up to date. */
  async flush(): Promise<void> {
    await this.chain;
  }

  private async writeRecord(record: JournalRecord, contentBytes: number): Promise<void> {
    const line = `${JSON.stringify(record)}\n`;
    await this.ensureDir();
    await fs.appendFile(this.filePath, line, { encoding: "utf8", mode: 0o600 });
    this.size += Buffer.byteLength(line, "utf8");
    if (this.size > this.maxBytes) {
      await this.compact(contentBytes);
    }
  }

  private async ensureDir(): Promise<void> {
    await fs.mkdir(path.dirname(this.filePath), { recursive: true });
  }

  /**
   * Drop the oldest records, replacing them with one gap covering the dropped
   * byte range. Keeps the largest suffix whose raw line bytes fit within half
   * the cap, leaving headroom so compaction does not trigger on every append.
   */
  private async compact(hintBytes: number): Promise<void> {
    const raw = await fs.readFile(this.filePath, "utf8");
    const lines = raw.split("\n").filter((line) => line.length > 0);
    const records = lines
      .map((line) => parseJournalLine(line))
      .filter((record): record is JournalRecord => record !== null);

    const budget = Math.max(Math.floor(this.maxBytes / 2), Math.min(hintBytes, 64 * 1024));
    let keptFrom = lines.length;
    let keptBytes = 0;
    for (let i = lines.length - 1; i >= 0; i--) {
      const lineBytes = Buffer.byteLength(lines[i], "utf8") + 1;
      if (keptBytes + lineBytes > budget && keptBytes > 0) {
        break;
      }
      keptBytes += lineBytes;
      keptFrom = i;
    }
    if (keptFrom <= 0) {
      // Nothing can be dropped; the cap is smaller than a single record.
      return;
    }

    const droppedFrom = records.slice(0, keptFrom).reduce(
      (acc, record) => Math.min(acc, recordStart(record)),
      Number.POSITIVE_INFINITY,
    );
    const droppedTo = records.slice(keptFrom).reduce(
      (acc, record) => Math.min(acc, recordStart(record)),
      this.offset,
    );

    const gap: JournalRecord = { kind: "gap", from: droppedFrom, to: droppedTo };
    const head = droppedFrom < droppedTo ? [`${JSON.stringify(gap)}\n`] : [];
    const rebuilt = [...head, ...lines.slice(keptFrom)].join("\n");
    // Rebuild with a trailing newline so future appends stay line-aligned.
    const content = rebuilt.length === 0 ? "" : `${rebuilt}\n`;

    await this.ensureDir();
    await writeFileAtomic(this.filePath, content);
    this.size = Buffer.byteLength(content, "utf8");
  }
}

/** Parse one journal line. Returns null for malformed lines so a reader can skip them. */
export function parseJournalLine(line: string): JournalRecord | null {
  try {
    const parsed = JSON.parse(line) as Partial<JournalRecord> & { kind?: string };
    if (parsed.kind === "out") {
      if (
        typeof parsed.offset !== "number" ||
        (parsed.stream !== "stdout" && parsed.stream !== "stderr") ||
        typeof parsed.text !== "string"
      ) {
        return null;
      }
      return { kind: "out", offset: parsed.offset, stream: parsed.stream, text: parsed.text };
    }
    if (parsed.kind === "gap") {
      if (typeof parsed.from !== "number" || typeof parsed.to !== "number") {
        return null;
      }
      return { kind: "gap", from: parsed.from, to: parsed.to };
    }
    return null;
  } catch {
    return null;
  }
}

/** The first byte offset a record covers. */
export function recordStart(record: JournalRecord): number {
  return record.kind === "out" ? record.offset : record.from;
}

/** The last byte offset a record covers (exclusive). */
export function recordEnd(record: JournalRecord): number {
  return record.kind === "out" ? record.offset + Buffer.byteLength(record.text, "utf8") : record.to;
}

/**
 * Read the current journal and return the records (or record tails) that cover
 * bytes at or beyond `from`, preserving order. Out records that straddle `from`
 * are trimmed to their unread tail.
 */
export async function readJournalFrom(stateRoot: string, executionId: string, from: number): Promise<JournalRecord[]> {
  let raw: string;
  try {
    raw = await fs.readFile(journalPathFor(stateRoot, executionId), "utf8");
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") {
      return [];
    }
    throw error;
  }
  return journalRecordsBeyond(raw, from);
}

/** Pure projection of a journal file's contents into the records beyond `from`. */
export function journalRecordsBeyond(raw: string, from: number): JournalRecord[] {
  const out: JournalRecord[] = [];
  for (const line of raw.split("\n")) {
    if (line.length === 0) {
      continue;
    }
    const record = parseJournalLine(line);
    if (!record) {
      continue;
    }
    if (record.kind === "out") {
      const end = recordEnd(record);
      if (end <= from) {
        continue;
      }
      if (record.offset >= from) {
        out.push(record);
      } else {
        out.push({ kind: "out", offset: from, stream: record.stream, text: record.text.slice(from - record.offset) });
      }
      continue;
    }
    if (record.to <= from) {
      continue;
    }
    out.push({ kind: "gap", from: Math.max(record.from, from), to: record.to });
  }
  return out;
}

function journalPathFor(stateRoot: string, executionId: string): string {
  return path.join(stateRoot, "executions", executionId, "journal");
}

async function writeFileAtomic(filePath: string, content: string): Promise<void> {
  const dir = path.dirname(filePath);
  await fs.mkdir(dir, { recursive: true });
  const tmp = `${filePath}.${process.pid}.${Date.now()}.tmp`;
  await fs.writeFile(tmp, content, { encoding: "utf8", mode: 0o600 });
  await fs.rename(tmp, filePath);
}
