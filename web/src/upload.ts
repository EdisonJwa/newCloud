// Uploads.
//
// Extracted from the Files view unchanged in behaviour, because two surfaces now start an
// upload: the Files page header and Home's upload action. Duplicating a resumable chunk pump
// in two places is how one of them quietly loses the resume.
//
// What is worth preserving, restated because it is the whole point of the file:
//
//   * **The server's ranges are authority.** localStorage is a hint that lets the interface
//     offer a resume; it is never evidence of which bytes landed. Every wave asks the status
//     endpoint first, and a conflict response (`409`) is not a failure but the answer to
//     "what do you already have".
//   * **A failed chunk is retried unchanged.** Guessing an offset after a network error is how
//     a hole is created; the server's merged set closes it on the next wave.
//   * **A dead session is not aborted on failure.** Aborting would throw away the staged bytes
//     and make the record useless — exactly the behaviour that made "resumable" false of the
//     product. The session expires on its own, and the person can discard it deliberately.
//   * **The browser's half survives the page.** A reload loses the `File` object, which is why
//     resuming means "pick the same file again"; the record keeps the session id, size and
//     mtime so a different file cannot be silently substituted.

import { api, APIError, type UploadRange } from "./api";
import type { FileNode } from "./types";

/**
 * Files above this size are sent in chunks so a dropped connection can resume. Below it the
 * single-request path is simpler and has no session to leave behind.
 */
export const CHUNK_THRESHOLD = 8 * 1024 * 1024;

/** How many times one chunk is retried before the whole upload is abandoned. */
const MAX_CHUNK_ATTEMPTS = 5;

/**
 * How many chunks one file may have in flight. Three keeps the connection busy on a
 * high-latency link without turning a modest file into a memory spike: each worker holds one
 * chunk-sized Blob slice plus whatever the fetch implementation buffers.
 */
const MAX_PARALLEL_CHUNKS = 3;

export interface PendingUpload {
  /** The server's upload session id. */
  id: string;
  dir: string;
  name: string;
  size: number;
  /** The File's lastModified, so a resumed upload cannot be fed a different file of the same name. */
  lastModified: number;
  /** Contiguous acknowledged prefix. The server's answer overrides it on resume. */
  offset: number;
  /**
   * Acknowledged intervals, including ranges beyond a hole. Optional because records written
   * by the sequential-only client do not have it; those still resume from `offset`, and the
   * status endpoint fills the set in before any chunk is sent.
   */
  ranges?: UploadRange[];
  /** The chunk size this session was started with, so a configured limit is not exceeded on resume. */
  chunk: number;
  updatedAt: number;
}

const PENDING_KEY = "wc.pendingUploads";

/** A week: long enough to come back to, short enough that the list cannot accumulate forever. */
const PENDING_MAX_AGE_MS = 7 * 24 * 60 * 60 * 1000;

function isUploadRange(value: unknown): value is UploadRange {
  if (typeof value !== "object" || value === null) return false;
  const r = value as Partial<UploadRange>;
  return (
    typeof r.start === "number" &&
    typeof r.end === "number" &&
    Number.isFinite(r.start) &&
    Number.isFinite(r.end) &&
    r.start >= 0 &&
    r.end > r.start
  );
}

/**
 * readPending returns the recorded sessions, dropping anything malformed or stale.
 *
 * Parsed field by field rather than cast: this is localStorage, which any other tab, any
 * browser extension and any earlier version of this code can write to.
 */
export function readPending(): PendingUpload[] {
  try {
    const raw = localStorage.getItem(PENDING_KEY);
    if (!raw) return [];
    const parsed: unknown = JSON.parse(raw);
    if (!Array.isArray(parsed)) return [];
    const cutoff = Date.now() - PENDING_MAX_AGE_MS;
    return parsed.filter((item): item is PendingUpload => {
      if (typeof item !== "object" || item === null) return false;
      const r = item as Partial<PendingUpload>;
      return (
        typeof r.id === "string" &&
        typeof r.dir === "string" &&
        typeof r.name === "string" &&
        typeof r.size === "number" &&
        typeof r.lastModified === "number" &&
        typeof r.offset === "number" &&
        (r.ranges === undefined || (Array.isArray(r.ranges) && r.ranges.every(isUploadRange))) &&
        typeof r.chunk === "number" &&
        typeof r.updatedAt === "number" &&
        r.updatedAt >= cutoff
      );
    });
  } catch {
    // Unreadable storage is not a reason to fail the view; it just means nothing can be resumed.
    return [];
  }
}

function writePending(list: PendingUpload[]): void {
  try {
    if (list.length === 0) localStorage.removeItem(PENDING_KEY);
    else localStorage.setItem(PENDING_KEY, JSON.stringify(list));
  } catch {
    /* private mode: resume across reloads is simply unavailable, uploads still work */
  }
}

/** rememberPending inserts or replaces one record by session id. */
function rememberPending(rec: PendingUpload): void {
  writePending([...readPending().filter((r) => r.id !== rec.id), rec]);
}

/** forgetPending drops a record that is finished, aborted or no longer known to the server. */
export function forgetPending(id: string): void {
  writePending(readPending().filter((r) => r.id !== id));
}

/** normalizeRanges clamps to the file, sorts, and merges touching/overlapping intervals. */
function normalizeRanges(ranges: UploadRange[] | undefined, size: number): UploadRange[] {
  if (!ranges) return [];
  const cleaned: UploadRange[] = [];
  for (const r of ranges) {
    if (!isUploadRange(r)) continue;
    const start = Math.max(0, Math.min(size, Math.floor(r.start)));
    const end = Math.max(0, Math.min(size, Math.floor(r.end)));
    if (end > start) cleaned.push({ start, end });
  }
  cleaned.sort((a, b) => a.start - b.start);
  const merged: UploadRange[] = [];
  for (const r of cleaned) {
    const last = merged[merged.length - 1];
    if (last && r.start <= last.end) {
      if (r.end > last.end) last.end = r.end;
    } else {
      merged.push({ ...r });
    }
  }
  return merged;
}

function sumRanges(ranges: UploadRange[]): number {
  return ranges.reduce((sum, r) => sum + (r.end - r.start), 0);
}

/** contiguousOffset is the acknowledged prefix: every byte below it is present. */
function contiguousOffset(ranges: UploadRange[]): number {
  let offset = 0;
  for (const r of ranges) {
    if (r.start > offset) break;
    if (r.end > offset) offset = r.end;
  }
  return offset;
}

function isCovered(ranges: UploadRange[], task: UploadRange): boolean {
  return ranges.some((r) => r.start <= task.start && r.end >= task.end);
}

function appendMissing(out: UploadRange[], from: number, to: number, chunk: number): void {
  for (let start = from; start < to; start += chunk) {
    out.push({ start, end: Math.min(start + chunk, to) });
  }
}

/** missingChunks is the set of chunk-sized ranges not covered by `ranges`, in file order. */
function missingChunks(size: number, ranges: UploadRange[], chunk: number): UploadRange[] {
  const width = Math.max(1, chunk);
  const out: UploadRange[] = [];
  let pos = 0;
  for (const r of ranges) {
    if (r.end <= pos) continue;
    if (r.start > pos) appendMissing(out, pos, Math.min(r.start, size), width);
    pos = Math.max(pos, r.end);
    if (pos >= size) break;
  }
  if (pos < size) appendMissing(out, pos, size, width);
  return out;
}

/**
 * runChunkWave sends `tasks` with at most MAX_PARALLEL_CHUNKS requests in flight.
 *
 * The workers share `onAccept`, which merges whatever the server reports. A conflict does not
 * retry the same range blindly: the server's ranges are merged, the wave ends, and the caller
 * recomputes what is missing. That keeps a partially-overlapping range from livelocking.
 */
async function runChunkWave(
  id: string,
  file: File,
  tasks: UploadRange[],
  getRanges: () => UploadRange[],
  onAccept: (ranges: UploadRange[]) => void,
): Promise<unknown | null> {
  let cursor = 0;
  let stopped = false;
  let fatal: unknown | null = null;

  const worker = async (): Promise<void> => {
    while (!stopped) {
      const index = cursor++;
      const task = tasks[index];
      if (!task) return;
      if (isCovered(getRanges(), task)) continue;

      let attempt = 0;
      while (!stopped) {
        try {
          const result = await api.putChunk(
            id,
            task.start,
            task.end - 1,
            file.size,
            file.slice(task.start, task.end),
          );
          if (stopped) return;
          if (result.conflict) {
            // A server that predates `ranges` reports only the contiguous offset; treating it
            // as [0,offset) is the same compatibility rule pumpChunks uses for a status response.
            onAccept(
              result.ranges.length > 0
                ? result.ranges
                : result.offset > 0
                  ? [{ start: 0, end: result.offset }]
                  : [],
            );
            break;
          }
          onAccept(result.ranges.length > 0 ? result.ranges : [task]);
          break;
        } catch (err) {
          if (err instanceof APIError && err.status === 404) {
            // Expired, aborted, or staging was cleaned up. The record cannot be resumed.
            forgetPending(id);
            stopped = true;
            fatal = err;
            return;
          }
          attempt += 1;
          if (attempt > MAX_CHUNK_ATTEMPTS) {
            // Deliberately NOT aborted. Aborting would discard the staged bytes and make the
            // record useless, which is exactly what it used to do: a long outage destroyed the
            // progress it was supposed to preserve. The session expires on its own, and the
            // discard action aborts it when the person decides not to continue.
            stopped = true;
            fatal = err;
            return;
          }
          // Retry the same range. If the server did receive it, the next response merges the
          // acknowledged set and the range is skipped.
          await new Promise((resolve) => setTimeout(resolve, Math.min(500 * attempt, 4000)));
        }
      }
    }
  };

  const workers = Math.min(MAX_PARALLEL_CHUNKS, tasks.length);
  await Promise.all(Array.from({ length: workers }, () => worker()));
  return fatal;
}

/**
 * pumpChunks sends every missing range, then publishes the file.
 *
 * Shared by a fresh upload and a resume because they are the same operation from different
 * starting states.
 */
async function pumpChunks(
  rec: PendingUpload,
  file: File,
  onProgress: (sent: number) => void,
): Promise<FileNode> {
  let chunk = rec.chunk > 0 ? rec.chunk : CHUNK_THRESHOLD;
  let ranges = normalizeRanges(rec.ranges, file.size);

  try {
    const status = await api.uploadStatus(rec.id);
    ranges = normalizeRanges(
      status.ranges && status.ranges.length > 0
        ? status.ranges
        : status.offset > 0
          ? [{ start: 0, end: status.offset }]
          : [],
      file.size,
    );
    if (status.chunk_size > 0) chunk = status.chunk_size;
  } catch (err) {
    if (err instanceof APIError) {
      if (err.status === 404) {
        forgetPending(rec.id);
        throw err;
      }
      if (err.status === 0 || err.status === 401 || err.status === 403) {
        throw err;
      }
    }
    // A transient status failure should not make a resumable upload fail; the local hint is a
    // safe starting point because every chunk is still checked against the server's answer.
  }
  chunk = Math.max(1, chunk);

  let record: PendingUpload = {
    ...rec,
    chunk,
    offset: contiguousOffset(ranges),
    ranges,
    updatedAt: Date.now(),
  };
  rememberPending(record);

  const accept = (incoming: UploadRange[]) => {
    ranges = normalizeRanges([...ranges, ...incoming], file.size);
    record = {
      ...record,
      offset: contiguousOffset(ranges),
      ranges,
      updatedAt: Date.now(),
    };
    rememberPending(record);
    onProgress(sumRanges(ranges));
  };
  onProgress(sumRanges(ranges));

  // A conflict can reveal a hole the local set did not know about, so recompute after every
  // wave. Eight is well past the number of rounds a correct server and client need; it exists
  // so a persistently refused chunk fails loudly instead of looping.
  for (let round = 0; round < 8; round++) {
    const missing = missingChunks(file.size, ranges, chunk);
    if (missing.length > 0) {
      const fatal = await runChunkWave(rec.id, file, missing, () => ranges, accept);
      if (fatal) throw fatal;
      continue;
    }

    try {
      const node = await api.completeUpload(rec.id);
      forgetPending(rec.id);
      return node;
    } catch (err) {
      // Finish re-derives completeness from the server's ranges. If it disagrees with us, its
      // answer wins and one more wave fills the difference.
      if (err instanceof APIError && err.status === 409 && err.ranges && err.ranges.length > 0) {
        accept(err.ranges);
        continue;
      }
      throw err;
    }
  }

  throw new APIError(0, "upload_incomplete", "the server did not acknowledge every range");
}

/**
 * uploadOne sends one file, using chunks when it is large enough to be worth resuming.
 *
 * A dropped connection on a 20 GiB file used to mean starting over. Now the retry loop resumes
 * from the server's acknowledged ranges, and a 409 is not a failure but the answer to "what do
 * you already have".
 */
export async function uploadOne(
  dir: string,
  file: File,
  onProgress: (sent: number) => void,
): Promise<FileNode> {
  if (file.size <= CHUNK_THRESHOLD) {
    const node = await api.upload(dir, file);
    onProgress(file.size);
    return node;
  }

  const session = await api.startUpload(dir, file.name, file.size);
  const rec: PendingUpload = {
    id: session.id,
    dir,
    name: file.name,
    size: file.size,
    lastModified: file.lastModified,
    offset: session.offset,
    ranges: normalizeRanges(session.ranges, file.size),
    chunk: session.chunk_size > 0 ? session.chunk_size : CHUNK_THRESHOLD,
    updatedAt: Date.now(),
  };
  rememberPending(rec);
  return pumpChunks(rec, file, onProgress);
}

/**
 * resumeUpload continues a session recorded before a reload.
 *
 * The File object is the one thing a browser cannot keep: a reload loses the bytes, so the
 * person has to pick the file again. What is checked is name + size + lastModified, and it is
 * worth being precise about what that does and does not prove:
 *
 *   CATCHES   the ordinary mistakes — a different file picked by accident, a renamed file, a
 *             file that was edited and saved since (its mtime moved), a truncation (its size
 *             moved).
 *
 *   DOES NOT  prove the bytes are identical. A file rewritten in place while preserving both
 *   CATCH     its length and its modification time is indistinguishable from the original here,
 *             and the server cannot help: it was told a name and a length and never saw the
 *             rest. Nor does anything in the record identify the ACCOUNT, so on a shared
 *             browser the list is whoever signed in last.
 */
export async function resumeUpload(
  rec: PendingUpload,
  file: File,
  onProgress: (sent: number) => void,
): Promise<FileNode> {
  if (file.size !== rec.size || file.lastModified !== rec.lastModified) {
    throw new APIError(0, "file_changed", "the selected file is not the one that was uploading");
  }
  return pumpChunks(rec, file, onProgress);
}

/** discard abandons every recorded session and releases its staged bytes. */
export async function discardPending(): Promise<void> {
  for (const rec of readPending()) {
    await api.abortUpload(rec.id);
  }
  writePending([]);
}
