// Files — the column browser.
//
// Each column is one level of the path. The path is not a string shown above the list; it is
// the list of columns to the left. That is the structural difference from every previous
// version of this interface: going deeper pushes a column, going back truncates, and clicking
// a file turns the last column into its detail.
//
// Why it is worth the complexity: the position of an item is remembered spatially rather than
// as a breadcrumb string, and the depth of a tree is visible at a glance. The cost is that a
// folder holding hundreds of files makes one column tall — acceptable, because a long single
// column is exactly what the previous list view was.

import {
  useCallback,
  useEffect,
  useMemo,
  useRef,
  useState,
  type DragEvent,
  type KeyboardEvent,
} from "react";
import { api, APIError, downloadURL, inlineURL, thumbnailURL, zipURL } from "../api";
import { useI18n } from "../i18n";
import { DeniedState, EmptyState, ErrorState, FileGlyph, Icon, Skeleton, useToast } from "../ui";
import {
  DeleteDialog,
  NewFolderDialog,
  RenameDialog,
  ShareDialog,
  TransferDialog,
  type TransferOp,
} from "../components/Dialogs";
import { Viewer } from "../components/Viewer";
import type { FileNode, Share, Version } from "../types";

interface Props {
  initialPath?: string;
  /** Bumped by the shell when something outside this view changed the tree. */
  reloadKey: number;
  onPathChange: (path: string) => void;
}

/**
 * Files above this size are sent in chunks so a dropped connection can resume. Below it the
 * single-request path is simpler and has no session to leak.
 */
const CHUNK_THRESHOLD = 8 * 1024 * 1024;

/** How many times one chunk is retried before the whole upload is abandoned. */
const MAX_CHUNK_ATTEMPTS = 5;

type ViewMode = "rows" | "grid";
type SortKey = "name" | "size" | "modified";
interface Sort {
  key: SortKey;
  dir: "asc" | "desc";
}

function readStored(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

function writeStored(key: string, value: string): void {
  try {
    localStorage.setItem(key, value);
  } catch {
    /* private mode: the preference simply does not persist */
  }
}

function readSort(): Sort {
  const raw = readStored("wc.sort");
  if (raw) {
    const [key, dir] = raw.split(":");
    if ((key === "name" || key === "size" || key === "modified") && (dir === "asc" || dir === "desc")) {
      return { key, dir };
    }
  }
  return { key: "name", dir: "asc" };
}

/* ---------- unfinished uploads ---------- */

/**
 * A chunked upload that has not finished yet.
 *
 * The server's half of a resumable upload already survives a refresh: the staged bytes and the
 * accepted offset live in the upload manager, not in the page. The BROWSER's half was a local
 * variable holding the session id, so closing the tab or reloading dropped the only reference to
 * work the server still had on disk. "Resumable" was then true of the protocol and false of the
 * product — the bytes were unreachable, and the session sat there until it expired.
 *
 * These records are that missing half. They are a HINT, never authority: every chunk response
 * carries the offset the server actually holds, and a session it no longer has answers 404, which
 * is treated as "upload it again" rather than as a retry.
 */
interface PendingUpload {
  /** The server's upload session id. */
  id: string;
  dir: string;
  name: string;
  size: number;
  /** The File's lastModified, so a resumed upload cannot be fed a different file of the same name. */
  lastModified: number;
  /** Where we believe we stopped. The server's answer overrides it on the first chunk. */
  offset: number;
  /** The chunk size this session was started with, so a configured limit is not exceeded on resume. */
  chunk: number;
  updatedAt: number;
}

const PENDING_KEY = "wc.pendingUploads";

/** A week: long enough to come back to, short enough that the list cannot accumulate forever. */
const PENDING_MAX_AGE_MS = 7 * 24 * 60 * 60 * 1000;

/** readPending returns the recorded sessions, dropping anything malformed or stale.
 *
 * Parsed field by field rather than cast: this is localStorage, which any other tab, any browser
 * extension and any earlier version of this code can write to.
 */
function readPending(): PendingUpload[] {
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
function forgetPending(id: string): void {
  writePending(readPending().filter((r) => r.id !== id));
}

// arrange filters and orders one column.
//
// Directories stay grouped first whatever the sort: they are the navigation, and a size sort
// that buries folders under every large file would break the one thing the column view is for.
function arrange(entries: FileNode[], needle: string, sort: Sort): FileNode[] {
  const query = needle.trim().toLowerCase();
  const rows = query ? entries.filter((e) => e.name.toLowerCase().includes(query)) : entries.slice();
  rows.sort((a, b) => {
    if ((a.kind === "dir") !== (b.kind === "dir")) return a.kind === "dir" ? -1 : 1;
    let cmp = 0;
    if (sort.key === "name") {
      cmp = a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: "base" });
    } else if (sort.key === "size") {
      cmp = a.size - b.size;
    } else {
      cmp = a.modified_at.localeCompare(b.modified_at);
    }
    // A stable tie-break keeps equal keys from reshuffling between renders.
    if (cmp === 0) cmp = a.name.localeCompare(b.name);
    return sort.dir === "asc" ? cmp : -cmp;
  });
  return rows;
}

/** Path helpers. The API addresses nodes by a slash-separated relative path. */
const leafOf = (p: string): string => p.slice(p.lastIndexOf("/") + 1) || "/";

export function Files({ initialPath = "", reloadKey, onPathChange }: Props) {
  const { t, fmt } = useI18n();
  const toast = useToast();

  // The stack of open directories, one per column. "" is the root.
  const [dirs, setDirs] = useState<string[]>(() => {
    if (!initialPath) return [""];
    const parts = initialPath.split("/").filter(Boolean);
    const acc = [""];
    for (const part of parts) acc.push(`${acc[acc.length - 1]}/${part}`.replace(/^\//, ""));
    return acc;
  });

  const [listings, setListings] = useState<Record<string, FileNode[]>>({});
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [loading, setLoading] = useState<Record<string, boolean>>({});
  const [sel, setSel] = useState<{ level: number; entry: FileNode } | null>(null);
  const [focusLevel, setFocusLevel] = useState(0);
  const [filter, setFilter] = useState("");
  const [view, setView] = useState<ViewMode>(() => (readStored("wc.view") === "grid" ? "grid" : "rows"));
  const [sort, setSort] = useState<Sort>(readSort);
  // picked holds ids in the deepest column. Selection in an ancestor column is a peek, not a
  // multi-selection, because bulk operations need one unambiguous target directory.
  const [picked, setPicked] = useState<string[]>([]);
  const [viewer, setViewer] = useState<{ entries: FileNode[]; index: number } | null>(null);
  const [dialog, setDialog] = useState<
    | { kind: "mkdir" }
    | { kind: "rename"; entry: FileNode }
    | { kind: "delete"; entries: FileNode[] }
    | { kind: "share"; entry: FileNode }
    | { kind: "transfer"; op: TransferOp; entries: FileNode[] }
    | null
  >(null);
  const [upload, setUpload] = useState<{
    done: number;
    total: number;
    bytesDone: number;
    bytesTotal: number;
  } | null>(null);
  const [dragging, setDragging] = useState(false);
  // Unfinished uploads recorded by a previous page load. Read once on mount: a session that was
  // interrupted in another tab is the same session, and the list is cheap to re-read after any
  // upload activity.
  const [pending, setPending] = useState<PendingUpload[]>(() => readPending());
  const [resuming, setResuming] = useState(false);

  const colRefs = useRef<Array<HTMLDivElement | null>>([]);
  const wantFocus = useRef(false);
  const fileInput = useRef<HTMLInputElement>(null);
  const resumeInput = useRef<HTMLInputElement>(null);
  const dragDepth = useRef(0);

  const deepest = dirs[dirs.length - 1] ?? "";

  /* ---------- loading ---------- */

  const load = useCallback(async (dir: string) => {
    setLoading((prev) => ({ ...prev, [dir]: true }));
    try {
      const res = await api.list(dir);
      setListings((prev) => ({ ...prev, [dir]: res.children ?? [] }));
      setErrors((prev) => {
        const next = { ...prev };
        delete next[dir];
        return next;
      });
    } catch (err) {
      const message = err instanceof APIError ? err.message || err.code : String(err);
      setErrors((prev) => ({ ...prev, [dir]: message }));
    } finally {
      setLoading((prev) => ({ ...prev, [dir]: false }));
    }
  }, []);

  // Every open column is fetched independently, so opening a deep path shows the outer levels
  // as soon as they arrive instead of waiting for the innermost one.
  useEffect(() => {
    for (const dir of dirs) {
      if (listings[dir] === undefined && !loading[dir] && errors[dir] === undefined) {
        void load(dir);
      }
    }
  }, [dirs, listings, loading, errors, load]);

  useEffect(() => {
    if (reloadKey > 0) {
      setListings({});
      setErrors({});
    }
  }, [reloadKey]);

  const reload = useCallback(() => {
    setListings({});
    setErrors({});
  }, []);

  /* ---------- navigation ---------- */

  const focusColumn = useCallback((level: number) => {
    wantFocus.current = true;
    setFocusLevel(level);
  }, []);

  useEffect(() => {
    if (!wantFocus.current) return;
    wantFocus.current = false;
    colRefs.current[Math.min(focusLevel, dirs.length - 1)]?.focus();
  }, [focusLevel, dirs, sel]);

  const openDir = useCallback(
    (level: number, entry: FileNode) => {
      setDirs((prev) => [...prev.slice(0, level + 1), entry.path]);
      setSel(null);
      setFilter("");
      focusColumn(level + 1);
    },
    [focusColumn],
  );

  const goUp = useCallback(
    (level: number) => {
      if (level <= 0) return;
      setDirs((prev) => prev.slice(0, level));
      setSel(null);
      setFilter("");
      focusColumn(level - 1);
    },
    [focusColumn],
  );

  const select = useCallback((level: number, entry: FileNode) => {
    setSel({ level, entry });
    setFocusLevel(level);
    setPicked(level === dirs.length - 1 ? [entry.id] : []);
  }, [dirs.length]);

  // onPick implements the click modifiers people expect from a file manager: shift extends a
  // range from the anchor, the platform modifier toggles one row, a plain click replaces.
  const onPick = useCallback(
    (level: number, entry: FileNode, e: React.MouseEvent) => {
      setFocusLevel(level);
      if (level !== dirs.length - 1) {
        setSel({ level, entry });
        setPicked([]);
        return;
      }

      const rows = arrange(listings[dirs[level] ?? ""] ?? [], filter, sort);

      if (e.shiftKey && sel && sel.level === level) {
        const from = rows.findIndex((x) => x.id === sel.entry.id);
        const to = rows.findIndex((x) => x.id === entry.id);
        if (from >= 0 && to >= 0) {
          const [lo, hi] = from < to ? [from, to] : [to, from];
          setPicked(rows.slice(lo, hi + 1).map((x) => x.id));
          return;
        }
      }

      if (e.metaKey || e.ctrlKey) {
        setPicked((prev) => (prev.includes(entry.id) ? prev.filter((id) => id !== entry.id) : [...prev, entry.id]));
        setSel({ level, entry });
        return;
      }

      setPicked([entry.id]);
      setSel({ level, entry });
    },
    [dirs, listings, filter, sort, sel],
  );

  const clearPicked = useCallback(() => {
    setPicked([]);
    setSel(null);
  }, []);

  // pickedEntries resolves the ids against the deepest listing, so a stale id (a file deleted
  // in another tab) simply drops out instead of producing an empty entry.
  const pickedEntries = useMemo(
    () => (listings[deepest] ?? []).filter((e) => picked.includes(e.id)),
    [listings, deepest, picked],
  );

  const open = useCallback(
    (level: number, entry: FileNode) => {
      if (entry.kind === "dir") {
        openDir(level, entry);
        return;
      }
      if (entry.preview) {
        const siblings = (listings[dirs[level] ?? ""] ?? []).filter((item) => item.kind === "file");
        const index = siblings.findIndex((item) => item.id === entry.id);
        setViewer({ entries: siblings, index: Math.max(0, index) });
      } else {
        window.location.href = downloadURL(entry.path);
      }
    },
    [dirs, listings, openDir],
  );

  useEffect(() => {
    onPathChange(deepest);
  }, [deepest, onPathChange]);

  /* ---------- uploads ---------- */

  const uploadFiles = useCallback(
    async (files: File[]) => {
      if (files.length === 0) return;
      if (upload) {
        toast.error(t("toast.uploading"));
        return;
      }
      const bytesTotal = files.reduce((sum, f) => sum + f.size, 0);
      setUpload({ done: 0, total: files.length, bytesDone: 0, bytesTotal });
      let done = 0;
      let failed = 0;
      let bytesDone = 0;

      for (const file of files) {
        const onProgress = (sent: number) => {
          setUpload({ done, total: files.length, bytesDone: bytesDone + sent, bytesTotal });
        };
        try {
          await uploadOne(deepest, file, onProgress);
          done += 1;
          bytesDone += file.size;
        } catch {
          failed += 1;
          bytesDone += file.size;
        }
        setUpload({ done: done + failed, total: files.length, bytesDone, bytesTotal });
      }
      setUpload(null);
      reload();
      setPending(readPending());
      if (failed === 0) {
        toast.info(t("toast.uploaded", { n: done }));
      } else {
        // Partial outcomes are reported as such; "uploaded" alone would be a lie.
        toast.error(t("toast.uploadSome", { done, failed }));
      }
    },
    [deepest, reload, toast, t, upload],
  );

  const onDrop = (e: DragEvent) => {
    e.preventDefault();
    dragDepth.current = 0;
    setDragging(false);
    const files = Array.from(e.dataTransfer.files ?? []);
    if (files.length) void uploadFiles(files);
  };

  /* ---------- resuming interrupted uploads ---------- */

  // resumePicked continues whichever recorded sessions the picked files belong to.
  //
  // The picker is the only way back to the bytes: the browser cannot persist a File, so "resume
  // after closing the tab" necessarily means "pick the same file again". Matching is by
  // name+size+lastModified, and a file that matches nothing is reported rather than uploaded from
  // scratch — starting a second upload of the same name would shadow the bytes already staged.
  const resumePicked = useCallback(
    async (files: File[]) => {
      if (files.length === 0) return;
      setResuming(true);
      let resumed = 0;
      try {
        for (const file of files) {
          const rec = readPending().find(
            (r) => r.name === file.name && r.size === file.size && r.lastModified === file.lastModified,
          );
          if (!rec) {
            toast.error(t("uploads.noMatch", { name: file.name }));
            continue;
          }
          try {
            await resumeUpload(rec, file, () => {});
            resumed += 1;
          } catch (err) {
            toast.error(
              err instanceof APIError && err.code === "file_changed"
                ? t("uploads.fileChanged", { name: file.name })
                : t("uploads.resumeFailed", { name: file.name }),
            );
          }
        }
      } finally {
        setPending(readPending());
        setResuming(false);
        // A resumed upload may have published a file, so the listing is stale either way.
        setListings({});
        setErrors({});
      }
      if (resumed > 0) toast.info(t("uploads.resumed", { n: resumed }));
    },
    [t, toast],
  );

  // discardPending abandons every recorded session and releases its staged bytes.
  //
  // Aborting is the deliberate version of what the upload loop used to do automatically: throw the
  // staging away. Here it is the person's decision, and it is the only way to stop a dead session
  // from occupying one of the account's upload slots.
  const discardPending = useCallback(async () => {
    for (const rec of readPending()) {
      await api.abortUpload(rec.id);
    }
    writePending([]);
    setPending([]);
    toast.info(t("uploads.discarded"));
  }, [t, toast]);

  /* ---------- keyboard ---------- */

  const onColumnKey = (e: KeyboardEvent<HTMLDivElement>, level: number) => {
    const dir = dirs[level] ?? "";
    const entries = arrange(listings[dir] ?? [], level === dirs.length - 1 ? filter : "", sort);
    const at = sel && sel.level === level ? entries.findIndex((x) => x.id === sel.entry.id) : -1;
    const move = (next: number) => {
      const entry = entries[Math.max(0, Math.min(next, entries.length - 1))];
      if (entry) select(level, entry);
    };

    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        move(at + 1);
        return;
      case "ArrowUp":
        e.preventDefault();
        move(at <= 0 ? 0 : at - 1);
        return;
      case "Home":
        e.preventDefault();
        move(0);
        return;
      case "End":
        e.preventDefault();
        move(entries.length - 1);
        return;
      case "ArrowRight":
        e.preventDefault();
        if (sel && sel.level === level) open(level, sel.entry);
        else if (level + 1 < dirs.length) focusColumn(level + 1);
        return;
      case "ArrowLeft":
        e.preventDefault();
        goUp(level);
        return;
      case "Enter":
        e.preventDefault();
        if (sel && sel.level === level) open(level, sel.entry);
        return;
      case "Backspace":
        e.preventDefault();
        goUp(level);
        return;
      case "F2":
        if (sel) {
          e.preventDefault();
          setDialog({ kind: "rename", entry: sel.entry });
        }
        return;
      case "Delete":
        e.preventDefault();
        if (pickedEntries.length > 0) {
          setDialog({ kind: "delete", entries: pickedEntries });
        } else if (sel) {
          setDialog({ kind: "delete", entries: [sel.entry] });
        }
        return;
      case "a":
        if ((e.metaKey || e.ctrlKey) && level === dirs.length - 1) {
          e.preventDefault();
          setPicked(entries.map((x) => x.id));
        }
        return;
      case "Escape":
        if (picked.length > 0) {
          e.preventDefault();
          clearPicked();
        }
        return;
      case " ": {
        // Space toggles the focused row, which is how a keyboard user builds a selection.
        if (level !== dirs.length - 1 || !sel || sel.level !== level) return;
        e.preventDefault();
        setPicked((prev) =>
          prev.includes(sel.entry.id) ? prev.filter((id) => id !== sel.entry.id) : [...prev, sel.entry.id],
        );
        return;
      }
    }
  };

  /* ---------- render ---------- */

  const activeError = errors[deepest];

  return (
    <>
      <div className="viewhead">
        <button
          type="button"
          className="btn primary"
          onClick={() => fileInput.current?.click()}
          disabled={upload !== null}
        >
          <Icon name="upload" size={13} />
          {t("files.upload")}
        </button>
        <button type="button" className="btn" onClick={() => setDialog({ kind: "mkdir" })}>
          <Icon name="folderPlus" size={13} />
          {t("files.newFolder")}
        </button>
        {upload ? (
          <span className="upload-progress">
            <span className="mono upload-count">
              {upload.done}/{upload.total}
            </span>
            {upload.bytesTotal > CHUNK_THRESHOLD ? (
              <progress
                className="meter inline"
                max={upload.bytesTotal}
                value={upload.bytesDone}
                aria-label={t("files.upload")}
              />
            ) : null}
          </span>
        ) : null}
        {pending.length > 0 ? (
          <div className="resume-bar" role="status">
            <Icon name="upload" size={12} />
            <span className="mono">{t("uploads.pending", { n: pending.length })}</span>
            {/* The picker is how the browser gets the bytes back. It is one control rather
                than a row per session: the person has the file, not the session id. */}
            <button
              type="button"
              className="btn sm"
              disabled={resuming}
              onClick={() => resumeInput.current?.click()}
            >
              {resuming ? t("uploads.resuming") : t("uploads.resume")}
            </button>
            <button
              type="button"
              className="btn sm ghost"
              disabled={resuming}
              onClick={() => void discardPending()}
            >
              {t("uploads.discard")}
            </button>
          </div>
        ) : null}
        {picked.length > 1 ? (
          <div className="selection-bar" role="status">
            <span className="mono">{t("files.selected", { n: picked.length })}</span>
            <button
              type="button"
              className="btn sm"
              onClick={() => setDialog({ kind: "transfer", op: "copy", entries: pickedEntries })}
            >
              <Icon name="copy" size={12} />
              {t("transfer.copy")}
            </button>
            <button
              type="button"
              className="btn sm"
              onClick={() => setDialog({ kind: "transfer", op: "move", entries: pickedEntries })}
            >
              <Icon name="move" size={12} />
              {t("transfer.move")}
            </button>
            {/* A plain link, not a fetch: the archive is streamed and the browser owns the
                download, its progress and its cancel. */}
            <a
              className="btn sm"
              href={zipURL(pickedEntries.map((e) => e.path))}
              download
              aria-label={t("transfer.zip")}
            >
              <Icon name="download" size={12} />
              {t("transfer.zip")}
            </a>
            <button
              type="button"
              className="btn sm danger"
              onClick={() => setDialog({ kind: "delete", entries: pickedEntries })}
            >
              <Icon name="trash" size={12} />
              {t("detail.delete")}
            </button>
            <button type="button" className="btn sm ghost" onClick={clearPicked}>
              {t("files.clearSelection")}
            </button>
          </div>
        ) : null}
        <span className="spacer" />
        <div className="seg" role="group" aria-label={t("files.view.label")}>
          <button
            type="button"
            aria-pressed={view === "rows"}
            aria-label={t("files.view.rows")}
            title={t("files.view.rows")}
            onClick={() => {
              setView("rows");
              writeStored("wc.view", "rows");
            }}
          >
            <Icon name="rows" size={13} />
          </button>
          <button
            type="button"
            aria-pressed={view === "grid"}
            aria-label={t("files.view.grid")}
            title={t("files.view.grid")}
            onClick={() => {
              setView("grid");
              writeStored("wc.view", "grid");
            }}
          >
            <Icon name="grid" size={13} />
          </button>
        </div>
        <select
          className="pick"
          value={`${sort.key}:${sort.dir}`}
          aria-label={t("files.sort.label")}
          onChange={(e) => {
            const [key, dir] = e.target.value.split(":");
            const next: Sort = { key: key as SortKey, dir: dir as "asc" | "desc" };
            setSort(next);
            writeStored("wc.sort", e.target.value);
          }}
        >
          <option value="name:asc">{t("files.sort.nameAsc")}</option>
          <option value="name:desc">{t("files.sort.nameDesc")}</option>
          <option value="modified:desc">{t("files.sort.newest")}</option>
          <option value="size:desc">{t("files.sort.largest")}</option>
          <option value="size:asc">{t("files.sort.smallest")}</option>
        </select>
        <div className="searchbox">
          <span className="icon">
            <Icon name="search" size={13} />
          </span>
          <input
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            placeholder={t("files.filter")}
            aria-label={t("files.filter")}
            onKeyDown={(e) => {
              if (e.key === "Escape") setFilter("");
            }}
          />
        </div>
      </div>

      {activeError ? (
        isDenied(activeError) ? (
          <DeniedState onHome={() => goUp(0)} />
        ) : (
          <ErrorState message={activeError} onRetry={() => void load(deepest)} />
        )
      ) : (
        <div
          className="cols"
          onDragEnter={(e) => {
            if (!e.dataTransfer.types.includes("Files")) return;
            dragDepth.current += 1;
            setDragging(true);
          }}
          onDragOver={(e) => {
            if (e.dataTransfer.types.includes("Files")) e.preventDefault();
          }}
          onDragLeave={() => {
            dragDepth.current = Math.max(0, dragDepth.current - 1);
            if (dragDepth.current === 0) setDragging(false);
          }}
          onDrop={onDrop}
        >
          <div className="collist">
            {dirs.map((dir, level) => {
            const isLast = level === dirs.length - 1;
            const entries = arrange(listings[dir] ?? [], isLast ? filter : "", sort);
            const loadingHere = loading[dir] && listings[dir] === undefined;
            const asGrid = isLast && view === "grid";

            return (
              <div key={dir || "/"} className={`col${isLast ? " active" : ""}`}>
                <div className="colhead">
                  {level > 0 ? (
                    <button type="button" onClick={() => goUp(level)} aria-label={t("files.up")}>
                      <Icon name="arrowUp" size={12} />
                    </button>
                  ) : null}
                  <span className="name" title={dir || "/"}>
                    {level === 0 ? t("nav.files") : leafOf(dir)}
                  </span>
                  <span className="count">{entries.length}</span>
                </div>
                {asGrid ? null : (
                  <div className="colsub" aria-hidden="true">
                    <span>{t("files.name")}</span>
                    <span>{t("files.size")}</span>
                  </div>
                )}

                <div
                  ref={(node) => {
                    colRefs.current[level] = node;
                  }}
                  className={asGrid ? "colbody colgrid" : "colbody"}
                  role="listbox"
                  tabIndex={0}
                  aria-label={level === 0 ? t("nav.files") : leafOf(dir)}
                  aria-activedescendant={
                    sel && sel.level === level ? `row-${level}-${sel.entry.id}` : undefined
                  }
                  onKeyDown={(e) => onColumnKey(e, level)}
                  onFocus={() => setFocusLevel(level)}
                >
                  {loadingHere ? <Skeleton /> : null}

                  {!loadingHere && entries.length === 0 ? (
                    errors[dir] ? (
                      <ErrorState message={errors[dir] ?? ""} onRetry={() => void load(dir)} />
                    ) : filter && isLast ? (
                      <EmptyState
                        icon="search"
                        title={t("files.noMatch")}
                        actions={
                          <button type="button" className="btn" onClick={() => setFilter("")}>
                            {t("files.clearFilter")}
                          </button>
                        }
                      />
                    ) : (
                      <EmptyState
                        icon="folderPlus"
                        title={t("files.emptyTitle")}
                        hint={t("files.emptyHint")}
                        actions={
                          <button type="button" className="btn primary" onClick={() => fileInput.current?.click()}>
                            {t("files.upload")}
                          </button>
                        }
                      />
                    )
                  ) : null}

                  {asGrid
                    ? entries.map((entry) => {
                        const chosen = picked.includes(entry.id);
                        return (
                          <div
                            key={entry.id}
                            id={`row-${level}-${entry.id}`}
                            role="option"
                            aria-selected={chosen}
                            className={`tile${entry.kind === "dir" ? " dir" : ""}`}
                            onClick={(e) => onPick(level, entry, e)}
                            onDoubleClick={() => open(level, entry)}
                          >
                            <span className="tileart">
                              {entry.preview === "image" ? (
                                <img src={thumbnailURL(entry.path)} alt="" loading="lazy" />
                              ) : (
                                <Icon name={entry.kind === "dir" ? "files" : "doc"} size={18} />
                              )}
                            </span>
                            <span className="tilename" title={entry.name}>
                              {entry.name}
                            </span>
                            <span className="tilesize">
                              {entry.kind === "dir" ? "—" : fmt.size(entry.size)}
                            </span>
                          </div>
                        );
                      })
                    : entries.map((entry) => {
                    const selected = sel?.level === level && sel.entry.id === entry.id;
                    return (
                      <div
                        key={entry.id}
                        id={`row-${level}-${entry.id}`}
                        role="option"
                        aria-selected={selected}
                        className={`row${entry.kind === "dir" ? " dir" : ""}`}
                        onClick={(e) => onPick(level, entry, e)}
                        onDoubleClick={() => open(level, entry)}
                        onKeyDown={(e) => {
                          if (e.key === "Enter") open(level, entry);
                        }}
                      >
                        <FileGlyph
                          entry={entry}
                          thumb={entry.preview === "image" ? thumbnailURL(entry.path) : undefined}
                        />
                        <span className="label" title={entry.name}>
                          {entry.name}
                        </span>
                        <span className="marks">
                          {entry.share_count ? (
                            <span className="m" title={t("detail.shared")}>
                              <Icon name="link" size={9} />
                              {entry.share_count}
                            </span>
                          ) : null}
                          {entry.has_versions ? (
                            <span className="m" title={t("detail.versions")}>
                              <Icon name="layers" size={9} />
                            </span>
                          ) : null}
                          {entry.kind === "file" ? <span className="m">{fmt.size(entry.size)}</span> : null}
                        </span>
                      </div>
                    );
                  })}
                </div>
              </div>
            );
            })}
          </div>

          {sel && picked.length <= 1 ? (
            <Detail
              key={sel.entry.id}
              entry={sel.entry}
              onClose={() => setSel(null)}
              onOpen={() => open(sel.level, sel.entry)}
              onRename={() => setDialog({ kind: "rename", entry: sel.entry })}
              onDelete={() => setDialog({ kind: "delete", entries: [sel.entry] })}
              onShare={() => setDialog({ kind: "share", entry: sel.entry })}
              onTransfer={(op) => setDialog({ kind: "transfer", op, entries: [sel.entry] })}
              onRefresh={reload}
            />
          ) : null}
        </div>
      )}

      {dragging ? (
        <div className="dropzone">
          <div className="box">
            {t("files.dropHere")}
            <small>{deepest ? `/${deepest}` : "/"}</small>
          </div>
        </div>
      ) : null}

      <input
        ref={fileInput}
        type="file"
        multiple
        hidden
        onChange={(e) => {
          const files = Array.from(e.target.files ?? []);
          e.target.value = "";
          void uploadFiles(files);
        }}
      />

      {/* The same control for a resume. Separate from the upload input so that picking a file to
          resume can never start a new upload: the two paths must not be one mis-click apart. */}
      <input
        ref={resumeInput}
        type="file"
        multiple
        hidden
        onChange={(e) => {
          const files = Array.from(e.target.files ?? []);
          e.target.value = "";
          void resumePicked(files);
        }}
      />

      {viewer ? (
        <Viewer
          entries={viewer.entries}
          index={viewer.index}
          onIndex={(index) => setViewer({ ...viewer, index })}
          onClose={() => setViewer(null)}
        />
      ) : null}

      {dialog?.kind === "mkdir" ? (
        <NewFolderDialog dir={deepest} onClose={() => setDialog(null)} onDone={reload} />
      ) : null}
      {dialog?.kind === "rename" ? (
        <RenameDialog entry={dialog.entry} onClose={() => setDialog(null)} onDone={reload} />
      ) : null}
      {dialog?.kind === "delete" ? (
        <DeleteDialog
          entries={dialog.entries}
          onClose={() => setDialog(null)}
          onDone={() => {
            setSel(null);
            reload();
          }}
        />
      ) : null}
      {dialog?.kind === "share" ? (
        <ShareDialog entry={dialog.entry} onClose={() => setDialog(null)} />
      ) : null}
      {dialog?.kind === "transfer" ? (
        <TransferDialog
          op={dialog.op}
          entries={dialog.entries}
          startDir={deepest}
          onClose={() => setDialog(null)}
          onDone={() => {
            setSel(null);
            setPicked([]);
            reload();
          }}
        />
      ) : null}
    </>
  );
}

/**
 * uploadOne sends one file, using chunks when it is large enough to be worth resuming.
 *
 * A dropped connection on a 20 GiB file used to mean starting over. Now the retry loop resumes
 * from whatever offset the server reports, and a 409 is not a failure but the answer to "where
 * was I" — which is why a failed chunk is retried unchanged rather than with a guessed offset.
 *
 * The session is recorded in localStorage before the first byte, so the resumability survives the
 * page rather than only the connection: see PendingUpload.
 */
async function uploadOne(
  dir: string,
  file: File,
  onProgress: (sent: number) => void,
): Promise<void> {
  if (file.size <= CHUNK_THRESHOLD) {
    await api.upload(dir, file);
    onProgress(file.size);
    return;
  }

  const session = await api.startUpload(dir, file.name, file.size);
  const rec: PendingUpload = {
    id: session.id,
    dir,
    name: file.name,
    size: file.size,
    lastModified: file.lastModified,
    offset: session.offset,
    chunk: session.chunk_size > 0 ? session.chunk_size : CHUNK_THRESHOLD,
    updatedAt: Date.now(),
  };
  rememberPending(rec);
  await pumpChunks(rec, file, onProgress);
}

/**
 * pumpChunks sends the bytes from rec.offset to the end and publishes the file.
 *
 * Shared by a fresh upload and a resume because they are the same operation from different
 * starting offsets — and because the retry rule has to be identical in both: a failed chunk is
 * retried unchanged, and a 409 is the server telling us where it actually is.
 */
async function pumpChunks(
  rec: PendingUpload,
  file: File,
  onProgress: (sent: number) => void,
): Promise<void> {
  const chunk = rec.chunk > 0 ? rec.chunk : CHUNK_THRESHOLD;
  let offset = rec.offset;
  let attempt = 0;

  while (offset < file.size) {
    const start = offset;
    const end = Math.min(start + chunk, file.size) - 1;
    try {
      const result = await api.putChunk(rec.id, start, end, file.size, file.slice(start, end + 1));
      offset = result.offset;
      // Recorded per chunk: a reload halfway through a large file should resume near the failure
      // rather than at the beginning.
      rememberPending({ ...rec, offset, updatedAt: Date.now() });
      onProgress(offset);
      attempt = 0;
    } catch (err) {
      if (err instanceof APIError && err.status === 404) {
        // The server no longer holds this session — it expired, or staging was cleaned up. The
        // record is dead, and keeping it would offer the person a resume that can never work.
        forgetPending(rec.id);
        throw err;
      }
      attempt += 1;
      if (attempt > MAX_CHUNK_ATTEMPTS) {
        // Deliberately NOT aborted. Aborting would discard the staged bytes and make the record
        // useless, which is exactly what it used to do: a long outage destroyed the progress it
        // was supposed to preserve. The session expires on its own, and the panel's discard
        // button aborts it when the person decides not to continue.
        throw err;
      }
      // Retry the same range. If the server did receive it, the next response is a conflict
      // carrying the true offset, and the loop continues from there.
      await new Promise((resolve) => setTimeout(resolve, Math.min(500 * attempt, 4000)));
    }
  }

  await api.completeUpload(rec.id);
  forgetPending(rec.id);
}

/**
 * resumeUpload continues a session recorded before a reload.
 *
 * The File object is the one thing a browser cannot keep: a reload loses the bytes, so the person
 * has to pick the file again. What is checked is name + size + lastModified, and it is worth being
 * precise about what that does and does not prove:
 *
 *   CATCHES   the ordinary mistakes — a different file picked by accident, a renamed file, a file
 *             that was edited and saved since (its mtime moved), a truncation (its size moved).
 *
 *   DOES NOT  prove the bytes are identical. A file rewritten in place while preserving both its
 *   CATCH     length and its modification time is indistinguishable from the original here, and the
 *             server cannot help: it was told a name and a length and never saw the rest. Nor does
 *             anything in the record identify the ACCOUNT, so on a shared browser the list is
 *             whoever signed in last.
 *
 * Closing that would need a content fingerprint (sampled hashing is a cheap approximation) and an
 * owner on the record. Until then this is a guard against the common mistake, not a cryptographic
 * identity check, and the staging TTL bounds how long a stale record can be offered at all.
 */
async function resumeUpload(
  rec: PendingUpload,
  file: File,
  onProgress: (sent: number) => void,
): Promise<void> {
  if (file.size !== rec.size || file.lastModified !== rec.lastModified) {
    throw new APIError(0, "file_changed", "the selected file is not the one that was uploading");
  }
  await pumpChunks(rec, file, onProgress);
}

/* ---------- detail ---------- */

interface DetailProps {
  entry: FileNode;
  onClose: () => void;
  onOpen: () => void;
  onRename: () => void;
  onDelete: () => void;
  onShare: () => void;
  onTransfer: (op: TransferOp) => void;
  onRefresh: () => void;
}

function Detail({ entry, onClose, onOpen, onRename, onDelete, onShare, onTransfer, onRefresh }: DetailProps) {
  const { t, fmt } = useI18n();
  const toast = useToast();
  const [versions, setVersions] = useState<Version[] | null>(null);
  // Whether the history below can actually be restored. The tree layout records version rows but
  // keeps no content for them, so the server reports version_content_available=false and refuses the
  // restore with 409. Offering the button anyway is a promise the deployment cannot keep (fourth
  // independent review, 6.1): the list is a record of changes, not recoverable content.
  const [versionsRestorable, setVersionsRestorable] = useState(false);
  const [shares, setShares] = useState<Share[] | null>(null);

  useEffect(() => {
    let live = true;
    if (entry.has_versions) {
      api
        .versions(entry.path)
        .then((res) => {
          if (!live) return;
          setVersions(res.versions ?? []);
          setVersionsRestorable(Boolean(res.version_content_available));
        })
        .catch(() => {
          if (!live) return;
          setVersions([]);
          setVersionsRestorable(false);
        });
    } else {
      setVersions([]);
      setVersionsRestorable(false);
    }
    api
      .shares()
      .then((res) => live && setShares((res.items ?? []).filter((s) => s.node_id === entry.id && !s.revoked_at)))
      .catch(() => live && setShares([]));
    return () => {
      live = false;
    };
  }, [entry.id, entry.path, entry.has_versions]);

  const kind =
    entry.kind === "dir" ? t("kind.folder") : entry.preview ? t(`kind.${entry.preview}`) : t("kind.file");

  return (
    <aside className="detail" aria-label={t("detail.title")}>
      <div className="stage">
        <Stage entry={entry} />
        {entry.preview ? (
          <button type="button" className="openhint" onClick={onOpen}>
            {t("detail.open")}
          </button>
        ) : null}
      </div>

      <div className="filehead">
        <button type="button" className="detail-close" onClick={onClose} aria-label={t("detail.close")}>
          <Icon name="close" size={12} />
        </button>
        <div className="n">{entry.name}</div>
        <div className="p">/{entry.path}</div>
      </div>

      <div className="scroll">
        <dl className="props">
          <dt>{t("detail.kind")}</dt>
          <dd>{kind}</dd>
          <dt>{t("detail.size")}</dt>
          <dd>{entry.kind === "dir" ? "—" : fmt.size(entry.size)}</dd>
          <dt>{t("detail.modified")}</dt>
          <dd>{fmt.full(entry.modified_at)}</dd>
          {entry.mime ? (
            <>
              <dt>{t("detail.type")}</dt>
              <dd>{entry.mime}</dd>
            </>
          ) : null}
        </dl>

        <div className="actions">
          {entry.kind === "file" ? (
            <a className="btn sm" href={downloadURL(entry.path)} download>
              <Icon name="download" size={12} />
              {t("detail.download")}
            </a>
          ) : null}
          <button type="button" className="btn sm" onClick={() => onTransfer("copy")}>
            <Icon name="copy" size={12} />
            {t("detail.copy")}
          </button>
          <button type="button" className="btn sm" onClick={() => onTransfer("move")}>
            <Icon name="move" size={12} />
            {t("detail.move")}
          </button>
          <button type="button" className="btn sm" onClick={onShare}>
            <Icon name="link" size={12} />
            {t("detail.share")}
          </button>
          <button type="button" className="btn sm" onClick={onRename}>
            <Icon name="edit" size={12} />
            {t("detail.rename")}
          </button>
          <button type="button" className="btn sm danger" onClick={onDelete}>
            <Icon name="trash" size={12} />
            {t("detail.delete")}
          </button>
        </div>

        <details className="section" open={Boolean(versions && versions.length)}>
          <summary>
            <Icon name="chevron" size={11} />
            <span className="micro">{t("detail.versions")}</span>
            <span className="n">{versions?.length ?? 0}</span>
          </summary>
          <div className="inner">
            {versions === null ? (
              <div className="hintline">{t("state.loading")}</div>
            ) : versions.length === 0 ? (
              <div className="hintline">{t("detail.versionsEmpty")}</div>
            ) : (
              <>
                {!versionsRestorable ? (
                  <div className="hintline">{t("detail.versionsNoContent")}</div>
                ) : null}
                {versions.map((v) => (
                  <div className="vrow" key={v.version}>
                    <span className="no">v{v.version}</span>
                    <span className="meta">
                      {fmt.size(v.size)} · {fmt.relative(v.created_at)}
                    </span>
                    {versionsRestorable ? (
                      <button
                        type="button"
                        className="btn sm act"
                        onClick={async () => {
                          try {
                            await api.restoreVersion(entry.path, v.version);
                            toast.info(t("toast.versionRestored", { n: v.version }));
                            onRefresh();
                          } catch (err) {
                            toast.error(err instanceof APIError ? err.message : String(err));
                          }
                        }}
                      >
                        {t("detail.restore")}
                      </button>
                    ) : null}
                  </div>
                ))}
              </>
            )}
          </div>
        </details>

        <details className="section" open={Boolean(shares && shares.length)}>
          <summary>
            <Icon name="chevron" size={11} />
            <span className="micro">{t("detail.shared")}</span>
            <span className="n">{shares?.length ?? 0}</span>
          </summary>
          <div className="inner">
            {shares === null ? (
              <div className="hintline">{t("state.loading")}</div>
            ) : shares.length === 0 ? (
              <div className="hintline">{t("detail.sharedEmpty")}</div>
            ) : (
              <>
                {shares.map((s) => (
                <div className="srow" key={s.id}>
                  <Icon name={s.kind === "request" ? "inbox" : "link"} size={11} />
                  <span className="srow-name">
                    {s.label || (s.kind === "request" ? t("home.request") : t("home.link"))}
                  </span>
                  <span className="uses">
                    {s.max_downloads ?? s.max_uploads
                      ? t("home.uses", {
                          used: s.download_count + s.upload_count,
                          max: s.max_downloads ?? s.max_uploads ?? 0,
                        })
                      : t("home.unlimited")}
                    </span>
                  </div>
                ))}
                {/* The token is stored only as a hash, so the URL genuinely cannot be shown
                    again. Saying so is more useful than a disabled button. */}
                <p className="hintline">{t("detail.linkNotRecoverable")}</p>
              </>
            )}
          </div>
        </details>
      </div>
    </aside>
  );
}

function Stage({ entry }: { entry: FileNode }) {
  const { t } = useI18n();
  const [text, setText] = useState<string | null>(null);
  const [broken, setBroken] = useState(false);

  useEffect(() => {
    setText(null);
    setBroken(false);
    if (entry.preview !== "text") return;
    let live = true;
    fetch(inlineURL(entry.path), { credentials: "same-origin" })
      .then((r) => (r.ok ? r.text() : Promise.reject(new Error(String(r.status)))))
      .then((body) => live && setText(body.slice(0, 2000)))
      .catch(() => live && setBroken(true));
    return () => {
      live = false;
    };
  }, [entry.path, entry.preview]);

  if (entry.kind === "dir") return <div className="note">{t("kind.folder")}</div>;
  if (broken) return <div className="note">{t("preview.unavailable")}</div>;

  switch (entry.preview) {
    case "image":
      return <img src={inlineURL(entry.path)} alt={entry.name} onError={() => setBroken(true)} />;
    case "text":
      return text === null ? <div className="note">{t("state.loading")}</div> : <pre>{text}</pre>;
    case "video":
    case "audio":
      return <div className="note">{t("kind.video")}</div>;
    case "pdf":
      return <div className="note">PDF</div>;
    default:
      return <div className="note">{t("detail.noPreview")}</div>;
  }
}

/* ---------- helpers ---------- */


function isDenied(message: string): boolean {
  const lower = message.toLowerCase();
  return lower.includes("not_found") || lower.includes("forbidden") || lower.includes("404");
}

