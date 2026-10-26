// Files.
//
// One listing. Folders are the first rows of the same table as the files: same height, same
// columns, same hover, same actions, distinguished by their tile and their item count. The
// previous structure put the folder hierarchy in a popup and then in a separate index, and
// both spent vertical budget before the first real file.
//
// The location is a breadcrumb built from real parent paths directly above the title. There is
// no folder picker: a folder is a row, a crumb jumps to an ancestor, and search finds a folder
// by name or by the path it lives on.
//
// ## State that is in the URL, and state that is not
//
// In the URL, because a link should open it: the folder (`space`), the filter (`kind`), the
// sort (`sort`, `dir`), the layout (`view`), the committed search (`q`), and whether the
// inspector is open (`inspector`, `pane`). In memory only: the live filter text before it is
// committed, the selection, the focused row and the upload queue. A selection is a moment, not
// a place, and putting 400 ids in a query string helps nobody.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  api,
  APIError,
  downloadURL,
  zipURL,
} from "../api";
import { useI18n } from "../i18n";
import {
  useAnnounce,
  Icon,
  FileTile,
  kindLabel,
  EmptyState,
  ErrorState,
  DeniedState,
  Skeleton,
  useToast,
  extensionOf,
} from "../ui";
import {
  DeleteDialog,
  NewFolderDialog,
  RenameDialog,
  ShareDialog,
  TransferDialog,
  type TransferOp,
} from "../components/Dialogs";
import {
  CHUNK_THRESHOLD,
  discardPending,
  readPending,
  resumeUpload,
  uploadOne,
  type PendingUpload,
} from "../upload";
import { Inspector } from "./Inspector";
import { TagChips } from "../components/Social";
import type { FileNode, Share } from "../types";
import type { KindFilter, Route, SortDir, SortKey, ViewMode } from "../route";
import { serialize } from "../route";

interface Props {
  route: Route;
  /** Bumped by the shell when something outside this view changed the tree. */
  reloadKey: number;
  onRoute: (changes: Partial<Route>, mode: "push" | "replace") => void;
  onOpenFile: (entry: FileNode, from: Route["from"]) => void;
  me: { id?: string; display_name?: string; username?: string; email?: string } | null;
  /** A request from elsewhere in the app to start an upload or a new folder. */
  pendingAction: "upload" | "mkdir" | null;
  onActionHandled: () => void;
}

/* --------------------------------------------------------------- sorting --- */

/**
 * arrange filters and orders one listing.
 *
 * Directories stay grouped first whatever the sort: they are the navigation, and a size sort
 * that buries folders under every large file would break the one thing the listing is for.
 * The grouping is a comparison term rather than a pre-sort so it composes with the direction.
 */
export function arrange(
  entries: FileNode[],
  needle: string,
  sort: { key: SortKey; dir: SortDir },
): FileNode[] {
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

/**
 * matchesKind applies the kind filter.
 *
 * The buckets are the ones a person actually thinks in, not the MIME taxonomy: Documents,
 * Images, Media and Other, with Folders as its own chip because a folder is the thing you are
 * usually looking for. The counts on the chips come from the same function, so a chip can
 * never promise a row the filter then hides.
 */
export function matchesKind(entry: FileNode, kind: KindFilter): boolean {
  if (kind === "all") return true;
  if (kind === "folders") return entry.kind === "dir";
  if (entry.kind === "dir") return false;

  const ext = extensionOf(entry.name).toLowerCase();
  switch (kind) {
    case "images":
      return entry.preview === "image";
    case "media":
      return (
        entry.preview === "video" ||
        entry.preview === "audio" ||
        ["mp4", "mov", "mkv", "webm", "mp3", "wav", "flac", "m4a", "ogg"].includes(ext)
      );
    case "documents":
      return (
        entry.preview === "pdf" ||
        entry.preview === "text" ||
        ["doc", "docx", "odt", "rtf", "txt", "md", "pdf", "pages", "xls", "xlsx", "csv", "tsv", "ods", "ppt", "pptx", "odp"].includes(
          ext,
        )
      );
    case "other":
      return (
        entry.preview === undefined && !["doc", "docx", "odt", "rtf", "txt", "md", "pdf", "jpg", "jpeg", "png", "gif", "webp", "svg", "mp4", "mov", "mp3"].includes(ext)
      );
    default:
      return true;
  }
}

const KIND_CHIPS: readonly { kind: KindFilter; key: string }[] = [
  { kind: "all", key: "files.filter.all" },
  { kind: "documents", key: "files.filter.documents" },
  { kind: "images", key: "files.filter.images" },
  { kind: "media", key: "files.filter.media" },
  { kind: "other", key: "files.filter.other" },
];

const SORTS: readonly { key: SortKey; dir: SortDir; label: string }[] = [
  { key: "name", dir: "asc", label: "files.sort.nameAsc" },
  { key: "name", dir: "desc", label: "files.sort.nameDesc" },
  { key: "modified", dir: "desc", label: "files.sort.newest" },
  { key: "modified", dir: "asc", label: "files.sort.oldest" },
  { key: "size", dir: "desc", label: "files.sort.largest" },
  { key: "size", dir: "asc", label: "files.sort.smallest" },
];

const leafOf = (p: string): string => p.slice(p.lastIndexOf("/") + 1) || "/";

/* ------------------------------------------------------------------ view --- */

export function Files({
  route,
  reloadKey,
  onRoute,
  onOpenFile,
  me,
  pendingAction,
  onActionHandled,
}: Props) {
  const { t, fmt } = useI18n();
  const toast = useToast();
  const { announce } = useAnnounce();

  const dir = route.space === "root" || route.space === "all" ? "" : route.space;

  const [entries, setEntries] = useState<FileNode[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  const [filter, setFilter] = useState("");
  const [picked, setPicked] = useState<string[]>([]);
  const [focusId, setFocusId] = useState<string | null>(null);
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
  const [dragTarget, setDragTarget] = useState<string | null>(null);
  const [moving, setMoving] = useState<{ entry: FileNode; x: number; y: number } | null>(null);
  const [pending, setPending] = useState<PendingUpload[]>(() => readPending());
  const [resuming, setResuming] = useState(false);
  const [shares, setShares] = useState<Share[] | null>(null);

  const listRef = useRef<HTMLDivElement>(null);
  const fileInput = useRef<HTMLInputElement>(null);
  const resumeInput = useRef<HTMLInputElement>(null);
  const dragDepth = useRef(0);
  const wantFocus = useRef(false);

  /* ---------- loading ---------- */

  // A label or a star is a CROSS-TREE filter, so it cannot be answered by listing one directory:
  // the tagged files may live anywhere, and filtering the current folder client-side would show an
  // empty list for a label that exists three levels down. Those two views go through the search
  // endpoint, which is the one place that can filter the whole tree.
  const filtered = route.tag !== "" || route.starred;

  const load = useCallback(async (path: string) => {
    setLoading(true);
    setError(null);
    try {
      if (filtered) {
        const res = await api.search({
          q: route.q,
          tag: route.tag || undefined,
          favorite: route.starred || undefined,
          kind: route.kind === "folders" ? "dir" : undefined,
          sort: route.sort,
          limit: 200,
        });
        setEntries(res.items ?? []);
        // The filter's scope is the whole tree, so "where" cannot be blank the way it is in a
        // folder listing: a row in a filtered view must say where its file lives or the user cannot
        // tell two same-named files apart.
        return;
      }
      const res = await api.list(path);
      setEntries(res.children ?? []);
    } catch (err) {
      setEntries(null);
      setError(err instanceof APIError ? err.message || err.code : String(err));
    } finally {
      setLoading(false);
    }
  }, [filtered, route.q, route.tag, route.starred, route.kind, route.sort]);

  useEffect(() => {
    void load(dir);
  }, [dir, reloadKey, load]);

  // The link inspector needs the share list only when it is showing a link, but fetching it
  // with the folder avoids a second round trip on the click that opens the pane.
  useEffect(() => {
    if (route.share && shares === null) {
      api
        .shares()
        .then((res) => setShares(res.items ?? []))
        .catch(() => setShares([]));
    }
  }, [route.share, shares]);

  const reload = useCallback(() => {
    void load(dir);
  }, [dir, load]);

  /* ---------- derived rows ---------- */

  // The filter text is committed to the URL only on Enter or blur; while it is being typed the
  // local value wins, so the list responds to every keystroke without a history entry per key.
  const needle = route.q || filter;

  const visible = useMemo(() => {
    const all = entries ?? [];
    return arrange(
      all.filter((e) => matchesKind(e, route.kind)),
      needle,
      { key: route.sort, dir: route.dir },
    );
  }, [entries, route.kind, route.sort, route.dir, needle]);

  const counts = useMemo(() => {
    const all = entries ?? [];
    const out: Record<KindFilter, number> = {
      all: all.length,
      folders: 0,
      documents: 0,
      images: 0,
      media: 0,
      other: 0,
    };
    for (const e of all) {
      if (e.kind === "dir") out.folders += 1;
      else if (matchesKind(e, "documents")) out.documents += 1;
      else if (matchesKind(e, "images")) out.images += 1;
      else if (matchesKind(e, "media")) out.media += 1;
      else out.other += 1;
    }
    return out;
  }, [entries]);

  const pickedEntries = useMemo(
    () => (entries ?? []).filter((e) => picked.includes(e.id)),
    [entries, picked],
  );

  /* ---------- navigation ---------- */

  const go = useCallback(
    (path: string) => {
      setPicked([]);
      setFocusId(null);
      setFilter("");
      onRoute({ space: path, q: "", share: "" }, "push");
    },
    [onRoute],
  );

  const crumbs = useMemo(() => {
    const out: { path: string; name: string }[] = [{ path: "", name: t("files.root") }];
    if (!dir) return out;
    const parts = dir.split("/").filter(Boolean);
    let acc = "";
    for (const part of parts) {
      acc = acc ? `${acc}/${part}` : part;
      out.push({ path: acc, name: part });
    }
    return out;
  }, [dir, t]);

  /* ---------- selection ---------- */

  const anchor = useRef<string | null>(null);

  const select = useCallback(
    (entry: FileNode, e: React.MouseEvent | null) => {
      setFocusId(entry.id);
      if (!e) {
        setPicked([entry.id]);
        anchor.current = entry.id;
        return;
      }
      if (e.shiftKey && anchor.current) {
        const from = visible.findIndex((x) => x.id === anchor.current);
        const to = visible.findIndex((x) => x.id === entry.id);
        if (from >= 0 && to >= 0) {
          const [lo, hi] = from < to ? [from, to] : [to, from];
          setPicked(visible.slice(lo, hi + 1).map((x) => x.id));
          return;
        }
      }
      if (e.metaKey || e.ctrlKey) {
        setPicked((prev) => (prev.includes(entry.id) ? prev.filter((id) => id !== entry.id) : [...prev, entry.id]));
        anchor.current = entry.id;
        return;
      }
      setPicked([entry.id]);
      anchor.current = entry.id;
    },
    [visible],
  );

  const clearPicked = useCallback(() => {
    setPicked([]);
    setFocusId(null);
    anchor.current = null;
  }, []);

  /* ---------- opening ---------- */

  const fromHere = useMemo<Route["from"]>(
    () =>
      needle
        ? { kind: "search", q: needle }
        : dir
          ? { kind: "folder", path: dir }
          : { kind: "folder", path: "" },
    [needle, dir],
  );

  const open = useCallback(
    (entry: FileNode) => {
      if (entry.kind === "dir") {
        go(entry.path);
        return;
      }
      // A file the server gives no preview for cannot be rendered; downloading it is the
      // honest action rather than opening an empty viewer.
      if (entry.preview) {
        onOpenFile(entry, fromHere);
      } else {
        window.location.href = downloadURL(entry.path);
      }
    },
    [go, onOpenFile, fromHere],
  );

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
          await uploadOne(dir, file, onProgress);
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
        const message = t("toast.uploaded", { n: done });
        toast.success(message);
        announce(message);
      } else {
        // Partial outcomes are reported as such; "uploaded" alone would be a lie.
        toast.error(t("toast.uploadSome", { done, failed }));
      }
    },
    [dir, reload, toast, t, upload, announce],
  );

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
        reload();
      }
      if (resumed > 0) toast.success(t("uploads.resumed", { n: resumed }));
    },
    [t, toast, reload],
  );

  useEffect(() => {
    if (pendingAction === "upload") {
      fileInput.current?.click();
      onActionHandled();
    } else if (pendingAction === "mkdir") {
      setDialog({ kind: "mkdir" });
      onActionHandled();
    }
  }, [pendingAction, onActionHandled]);

  /* ---------- drag: OS files in, rows onto folders ---------- */

  const onDragEnter = (e: React.DragEvent) => {
    if (!e.dataTransfer.types.includes("Files")) return;
    dragDepth.current += 1;
    setDragging(true);
  };

  const onDragLeave = () => {
    dragDepth.current = Math.max(0, dragDepth.current - 1);
    if (dragDepth.current === 0) setDragging(false);
  };

  const onDrop = (e: React.DragEvent) => {
    e.preventDefault();
    dragDepth.current = 0;
    setDragging(false);
    const files = Array.from(e.dataTransfer.files ?? []);
    if (files.length) void uploadFiles(files);
  };

  // Moving a row onto a folder is a pointer gesture, not an HTML5 drag: the row is a grid cell
  // whose children include buttons and a menu, and `draggable` on it makes text selection and
  // the menu unusable. A pointer-down with a 4px threshold keeps the two apart.
  // `id` is not needed: the pointer listeners are on the window and check `e.buttons`, so a
  // second pointer arriving mid-drag cancels rather than fighting for the ghost.
  const moveStart = useRef<{ entry: FileNode; x: number; y: number } | null>(null);

  const onRowPointerDown = (
    entry: FileNode,
    e: { button: number; clientX: number; clientY: number; target: EventTarget },
  ) => {
    if (e.button !== 0 || entry.kind === "dir") return;
    if ((e.target as HTMLElement).closest("button, a, input, summary, label")) return;
    moveStart.current = { entry, x: e.clientX, y: e.clientY };
  };

  useEffect(() => {
    const onMove = (e: PointerEvent) => {
      const start = moveStart.current;
      if (!start) return;
      const dx = Math.abs(e.clientX - start.x);
      const dy = Math.abs(e.clientY - start.y);
      if (!moving && Math.max(dx, dy) < 5) return;
      if (e.buttons === 0) {
        moveStart.current = null;
        setMoving(null);
        setDragTarget(null);
        return;
      }
      setMoving({ entry: start.entry, x: e.clientX, y: e.clientY });
      const el = document.elementFromPoint(e.clientX, e.clientY);
      const row = el?.closest<HTMLElement>("[data-drop-folder]");
      const target = row?.dataset.dropFolder ?? null;
      setDragTarget(target && target !== start.entry.path ? target : null);
    };
    const onUp = async () => {
      const start = moveStart.current;
      const target = dragTarget;
      moveStart.current = null;
      setMoving(null);
      setDragTarget(null);
      if (!start || !target) return;
      try {
        await api.batch({ op: "move", items: [{ from: start.entry.path, to: target }] });
        toast.success(t("toast.moved", { name: start.entry.name }));
        setPicked([]);
        reload();
      } catch (err) {
        toast.error(err instanceof APIError ? err.message || err.code : String(err));
      }
    };
    window.addEventListener("pointermove", onMove);
    window.addEventListener("pointerup", onUp);
    window.addEventListener("pointercancel", onUp);
    return () => {
      window.removeEventListener("pointermove", onMove);
      window.removeEventListener("pointerup", onUp);
      window.removeEventListener("pointercancel", onUp);
    };
  }, [dragTarget, moving, reload, t, toast]);

  /* ---------- keyboard ---------- */

  // One `role="listbox"` with a moving `aria-activedescendant` rather than 400 tab stops: a
  // keyboard user reaches the list once and then arrows through it, which is what a file
  // manager has to do to be usable without a mouse.
  const onListKey = (e: React.KeyboardEvent) => {
    const at = focusId ? visible.findIndex((x) => x.id === focusId) : -1;
    const moveTo = (next: number) => {
      const entry = visible[Math.max(0, Math.min(next, visible.length - 1))];
      if (!entry) return;
      setFocusId(entry.id);
      if (e.shiftKey) {
        setPicked((prev) => (prev.includes(entry.id) ? prev : [...prev, entry.id]));
      } else {
        setPicked([entry.id]);
        anchor.current = entry.id;
      }
    };

    switch (e.key) {
      case "ArrowDown":
        e.preventDefault();
        moveTo(at + 1);
        return;
      case "ArrowUp":
        e.preventDefault();
        moveTo(at <= 0 ? 0 : at - 1);
        return;
      case "Home":
        e.preventDefault();
        moveTo(0);
        return;
      case "End":
        e.preventDefault();
        moveTo(visible.length - 1);
        return;
      case "ArrowRight":
      case "Enter": {
        const entry = focusId ? visible.find((x) => x.id === focusId) : null;
        if (!entry) return;
        e.preventDefault();
        open(entry);
        return;
      }
      case " ":
        // Space toggles the focused row, which is how a keyboard user builds a selection.
        if (!focusId) return;
        e.preventDefault();
        setPicked((prev) =>
          prev.includes(focusId) ? prev.filter((id) => id !== focusId) : [...prev, focusId],
        );
        return;
      case "F2": {
        const entry = focusId ? visible.find((x) => x.id === focusId) : null;
        if (!entry) return;
        e.preventDefault();
        setDialog({ kind: "rename", entry });
        return;
      }
      case "Delete":
      case "Backspace": {
        if (pickedEntries.length === 0) {
          // Backspace with nothing selected is "up one level", the gesture people bring from
          // every file manager; it is intercepted only when it would delete something.
          if (e.key === "Backspace" && dir) {
            e.preventDefault();
            go(crumbs[crumbs.length - 2]?.path ?? "");
          }
          return;
        }
        e.preventDefault();
        setDialog({ kind: "delete", entries: pickedEntries });
        return;
      }
      case "Escape":
        if (picked.length > 0) {
          e.preventDefault();
          clearPicked();
        }
        return;
      case "a":
        if (e.metaKey || e.ctrlKey) {
          e.preventDefault();
          setPicked(visible.map((x) => x.id));
        }
        return;
      default:
        break;
    }
  };

  useEffect(() => {
    if (!wantFocus.current) return;
    wantFocus.current = false;
    listRef.current?.focus();
  }, [dir]);

  /* ---------- render ---------- */

  const sortLabel = t(
    SORTS.find((s) => s.key === route.sort && s.dir === route.dir)?.label ?? "files.sort.nameAsc",
  );

  const title = dir ? leafOf(dir) : t("files.allTitle");
  // Two pluralised clauses rather than "N items · M folders": each count selects its own form,
  // which is what makes "1 folder" and "3 folders" both correct in a language with more than
  // two categories. The sentence is assembled from two whole phrases, never from fragments.
  const description = useMemo(() => {
    const total = entries?.length ?? 0;
    const folders = entries?.filter((e) => e.kind === "dir").length ?? 0;
    return t("files.description", {
      items: t("files.items", { n: total }),
      folders: t("files.folderCount", { n: folders }),
    });
  }, [entries, t]);

  const selectedShare =
    route.share && shares ? (shares.find((s) => s.id === route.share) ?? null) : null;

  return (
    <div
      className="serein-page serein-page--files wc-page--fill"
      onDragEnter={onDragEnter}
      onDragOver={(e) => {
        if (e.dataTransfer.types.includes("Files")) e.preventDefault();
      }}
      onDragLeave={onDragLeave}
      onDrop={onDrop}
    >
      {/*
        The trail shows ANCESTORS only. The folder you are in is the page title directly below,
        so repeating it here was an eyebrow that said the same word twice. At the root there is
        no ancestor and the trail disappears entirely — the title carries the location.
      */}
      {crumbs.length > 1 ? (
        <nav className="serein-breadcrumb wc-crumbs" aria-label={t("files.breadcrumb")}>
          <ol className="serein-breadcrumb__list">
            {crumbs.slice(0, -1).map((crumb, i, ancestors) => {
              // On a phone only the immediate parent survives: the full trail is what pushed the
              // title and the first row off the fold in the version this replaces.
              const hide = ancestors.length > 1 && i < ancestors.length - 2;
              return (
                <li
                  key={crumb.path || "root"}
                  className={`serein-breadcrumb__item${hide ? " wc-crumbs__ancestor" : ""}`}
                >
                  <a
                    className="serein-breadcrumb__link"
                    href={serialize({ ...route, screen: "files", space: crumb.path, at: 0 })}
                    onClick={(e) => {
                      e.preventDefault();
                      go(crumb.path);
                    }}
                  >
                    {crumb.name}
                  </a>
                  <span className="serein-breadcrumb__sep" aria-hidden="true">
                    <Icon name="chevron" size={11} />
                  </span>
                </li>
              );
            })}
          </ol>
        </nav>
      ) : null}

      <div className="serein-page-header">
        <div className="serein-page-header__main">
          <h1 className="serein-page-header__title">{title}</h1>
          <p className="serein-page-header__description">{description}</p>
        </div>
        <div className="serein-page-header__actions">
          {/*
            Both actions carry an `aria-label`. On a phone the New folder button collapses to
            its icon (the label is hidden by CSS to keep the header one row tall), and a hidden
            label is not an accessible name — the control would announce as an unlabelled
            button. The `aria-label` is the same string the visible label uses, so the two can
            never disagree.
          */}
          <button
            type="button"
            className="serein-btn serein-btn--outline"
            aria-label={t("files.newFolder")}
            title={t("files.newFolder")}
            onClick={() => setDialog({ kind: "mkdir" })}
          >
            <Icon name="folderPlus" size={13} />
            <span className="serein-btn__label">{t("files.newFolder")}</span>
          </button>
          <button
            type="button"
            className="serein-btn"
            aria-label={t("files.upload")}
            onClick={() => fileInput.current?.click()}
            disabled={upload !== null}
          >
            <Icon name="upload" size={13} />
            <span className="serein-btn__label">{t("files.upload")}</span>
          </button>
        </div>
      </div>

      {upload ? (
        <div className="wc-progress-inline" role="status">
          <span className="serein-spinner" aria-hidden="true" />
          <span>{t("files.uploading", { done: upload.done, total: upload.total })}</span>
          {upload.bytesTotal > CHUNK_THRESHOLD ? (
            <progress
              className="wc-meter"
              max={upload.bytesTotal}
              value={upload.bytesDone}
              aria-label={t("files.uploadProgress")}
            />
          ) : null}
        </div>
      ) : null}

      {pending.length > 0 ? (
        <div className="wc-resumebar" role="status">
          <Icon name="upload" size={13} />
          <span>{t("uploads.pending", { n: pending.length })}</span>
          {/* The picker is how the browser gets the bytes back. It is one control rather than
              a row per session: the person has the file, not the session id. */}
          <button
            type="button"
            className="serein-btn serein-btn--sm serein-btn--outline"
            disabled={resuming}
            onClick={() => resumeInput.current?.click()}
          >
            {resuming ? t("uploads.resuming") : t("uploads.resume")}
          </button>
          <button
            type="button"
            className="serein-btn serein-btn--sm serein-btn--quiet"
            disabled={resuming}
            onClick={() => {
              // Discard throws away every resumable session at once, and the files on the
              // caller's disk are the only copy of what was half-uploaded — confirm first.
              if (!window.confirm(t("uploads.discardConfirm", { n: pending.length }))) return;
              void discardPending().then(() => {
                setPending([]);
                toast.info(t("uploads.discarded"));
              });
            }}
          >
            {t("uploads.discard")}
          </button>
        </div>
      ) : null}

      <div className="serein-actionbar">
        <div className="serein-actionbar__filters" role="group" aria-label={t("files.filterLabel")}>
          {KIND_CHIPS.map((chip) => (
            <button
              key={chip.kind}
              type="button"
              className="serein-filter"
              aria-pressed={route.kind === chip.kind}
              onClick={() => onRoute({ kind: chip.kind }, "replace")}
            >
              {t(chip.key)}
              <span className="serein-filter__count">{counts[chip.kind]}</span>
            </button>
          ))}
        </div>

        <div className="serein-actionbar__tools">
          <label className="serein-search wc-filesearch">
            <span className="serein-sr-only">{t("files.filter")}</span>
            <span className="serein-input-wrap serein-input-wrap--sm">
              <span className="serein-input-wrap__leading">
                <Icon name="search" size={12} />
              </span>
              <input
                className="serein-input"
                type="text"
                value={filter}
                placeholder={t("files.filter")}
                onChange={(e) => setFilter(e.target.value)}
                onKeyDown={(e) => {
                  if (e.key === "Enter") {
                    onRoute({ q: filter.trim() }, "push");
                  } else if (e.key === "Escape") {
                    setFilter("");
                    if (route.q) onRoute({ q: "" }, "replace");
                  }
                }}
                onBlur={() => {
                  if (filter.trim() && filter.trim() !== route.q) onRoute({ q: filter.trim() }, "push");
                }}
              />
              {filter ? (
                <button
                  type="button"
                  className="serein-input-wrap__trailing"
                  aria-label={t("files.clearFilter")}
                  onClick={() => {
                    setFilter("");
                    if (route.q) onRoute({ q: "" }, "replace");
                  }}
                >
                  <Icon name="close" size={11} />
                </button>
              ) : null}
            </span>
          </label>

          <details className="serein-menu">
            <summary className="serein-sort" aria-label={t("files.sortBy", { value: sortLabel })}>
              <Icon name="rows" size={12} />
              <span className="serein-sort__label">{sortLabel}</span>
              <Icon name="chevronDown" size={11} />
            </summary>
            <div className="serein-menu__surface serein-menu__surface--start" role="menu">
              {SORTS.map((s) => (
                <button
                  key={`${s.key}-${s.dir}`}
                  type="button"
                  className="serein-menu__item"
                  role="menuitemradio"
                  aria-checked={route.sort === s.key && route.dir === s.dir}
                  onClick={() => onRoute({ sort: s.key, dir: s.dir }, "replace")}
                >
                  <span className="serein-menu__icon">
                    {route.sort === s.key && route.dir === s.dir ? <Icon name="check" size={12} /> : null}
                  </span>
                  {t(s.label)}
                </button>
              ))}
            </div>
          </details>

          <div className="serein-seg" role="group" aria-label={t("files.view.label")}>
            <button
              type="button"
              className="serein-seg__btn"
              aria-pressed={route.view === "rows"}
              aria-label={t("files.view.rows")}
              onClick={() => onRoute({ view: "rows" }, "replace")}
            >
              <Icon name="rows" size={13} />
            </button>
            <button
              type="button"
              className="serein-seg__btn"
              aria-pressed={route.view === "grid"}
              aria-label={t("files.view.grid")}
              onClick={() => onRoute({ view: "grid" }, "replace")}
            >
              <Icon name="grid" size={13} />
            </button>
          </div>
        </div>
      </div>

      {picked.length > 1 ? (
        <div className="serein-selectionbar serein-selectionbar--inline wc-selection" role="status">
          <span className="serein-selectionbar__count">
            <strong>{picked.length}</strong> {t("files.selectedLabel")}
          </span>
          <div className="serein-selectionbar__actions">
            <button
              type="button"
              className="serein-btn serein-btn--sm serein-btn--quiet"
              onClick={() => setDialog({ kind: "transfer", op: "copy", entries: pickedEntries })}
            >
              <Icon name="copy" size={12} />
              {t("transfer.copy")}
            </button>
            <button
              type="button"
              className="serein-btn serein-btn--sm serein-btn--quiet"
              onClick={() => setDialog({ kind: "transfer", op: "move", entries: pickedEntries })}
            >
              <Icon name="move" size={12} />
              {t("transfer.move")}
            </button>
            {/* A plain link, not a fetch: the archive is streamed and the browser owns the
                download, its progress and its cancel. */}
            <a
              className="serein-btn serein-btn--sm serein-btn--quiet"
              href={zipURL(pickedEntries.map((e) => e.path))}
              download
            >
              <Icon name="download" size={12} />
              {t("transfer.zip")}
            </a>
            <button
              type="button"
              className="serein-btn serein-btn--sm serein-btn--quiet"
              onClick={() => setDialog({ kind: "delete", entries: pickedEntries })}
            >
              <Icon name="trash" size={12} />
              {t("detail.delete")}
            </button>
            <button type="button" className="serein-btn serein-btn--sm" onClick={clearPicked}>
              {t("files.clearSelection")}
            </button>
          </div>
        </div>
      ) : null}

      <div className="wc-split" data-inspector={route.inspector && !selectedShare ? "docked" : "closed"}>
        <div className="wc-files-main">
          {error ? (
            isDenied(error) ? (
              <DeniedState onHome={() => go("")} />
            ) : (
              <ErrorState message={error} onRetry={reload} />
            )
          ) : loading && entries === null ? (
            <Skeleton rows={8} />
          ) : visible.length === 0 ? (
            <EmptyState
              icon={needle ? "search" : "folderPlus"}
              title={needle ? t("files.noMatch") : t("files.emptyTitle")}
              hint={needle ? t("files.noMatchHint", { q: needle }) : t("files.emptyHint")}
              actions={
                needle ? (
                  <button
                    type="button"
                    className="serein-btn serein-btn--outline"
                    onClick={() => {
                      setFilter("");
                      onRoute({ q: "" }, "replace");
                    }}
                  >
                    {t("files.clearFilter")}
                  </button>
                ) : (
                  <button type="button" className="serein-btn" onClick={() => fileInput.current?.click()}>
                    {t("files.upload")}
                  </button>
                )
              }
            />
          ) : route.view === "grid" ? (
            <GridView
              rows={visible}
              picked={picked}
              focusId={focusId}
              onPick={select}
              onOpen={open}
            />
          ) : (
            <ListView
              rows={visible}
              picked={picked}
              focusId={focusId}
              listRef={listRef}
              onPick={select}
              onOpen={open}
              onKey={onListKey}
              onMenu={(entry) => {
                setFocusId(entry.id);
                setPicked([entry.id]);
              }}
              onAction={(kind, entry) => {
                if (kind === "rename") setDialog({ kind: "rename", entry });
                else if (kind === "delete") setDialog({ kind: "delete", entries: [entry] });
                else if (kind === "share") setDialog({ kind: "share", entry });
                else if (kind === "copy") setDialog({ kind: "transfer", op: "copy", entries: [entry] });
                else if (kind === "move") setDialog({ kind: "transfer", op: "move", entries: [entry] });
                else if (kind === "star") {
                  // The row's star is optimistic at the source: the click must not wait for a round
                  // trip. A refusal reloads the listing, which is what puts the row back to whatever
                  // the server actually holds.
                  const next = !entry.starred;
                  setEntries((prev) =>
                    prev ? prev.map((e) => (e.id === entry.id ? { ...e, starred: next } : e)) : prev,
                  );
                  (next ? api.star({ id: entry.id }) : api.unstar({ id: entry.id })).catch((err) => {
                    toast.error(err instanceof APIError ? err.message || err.code : String(err));
                    reload();
                  });
                } else if (kind === "inspector") {
                  setFocusId(entry.id);
                  setPicked([entry.id]);
                  if (!route.inspector) onRoute({ inspector: true }, "push");
                }
              }}
              onPointerDownRow={onRowPointerDown}
              dragTarget={dragTarget}
              onDropOnRow={async (entry) => {
                const source = moving?.entry ?? moveStart.current?.entry;
                if (!source || entry.kind !== "dir" || entry.path === source.path) return;
                try {
                  await api.batch({ op: "move", items: [{ from: source.path, to: entry.path }] });
                  toast.success(t("toast.moved", { name: source.name }));
                  setPicked([]);
                  reload();
                } catch (err) {
                  toast.error(err instanceof APIError ? err.message || err.code : String(err));
                }
              }}
              onSort={(key) => {
                // Clicking the active column flips the direction; a new column starts in the
                // direction that is useful for it rather than always ascending.
                if (route.sort === key) {
                  onRoute({ dir: route.dir === "asc" ? "desc" : "asc" }, "replace");
                } else {
                  onRoute({ sort: key, dir: key === "name" ? "asc" : "desc" }, "replace");
                }
              }}
              sort={route.sort}
              dir={route.dir}
              shares={shares}
              onFilterTag={(tag) => {
                // Filtering by a label is a SEARCH, not a listing filter: the tagged files may live
                // anywhere in the tree, and a client-side filter over the current folder would show
                // an empty list for a label that exists on files one level away.
                onRoute({ space: "all", q: "", tag: tag.id }, "push");
              }}
              t={t}
              fmt={fmt}
            />
          )}

          {needle && visible.length > 0 ? (
            <p className="wc-filterline">
              {t("files.filteredBy", { q: needle, n: visible.length })}{" "}
              {route.q ? (
                <button
                  type="button"
                  className="serein-link"
                  onClick={() => onRoute({ q: "" }, "replace")}
                >
                  {t("files.clearFilter")}
                </button>
              ) : null}
            </p>
          ) : null}
        </div>

        {route.inspector ? (
          <Inspector
            mode="file"
            entry={
              picked.length === 1
                ? ((entries ?? []).find((e) => e.id === picked[0]) ?? null)
                : picked.length === 0 && focusId
                  ? ((entries ?? []).find((e) => e.id === focusId) ?? null)
                  : null
            }
            selectionCount={picked.length}
            pane={route.pane}
            onPane={(pane) => onRoute({ pane }, "replace")}
            onClose={() => onRoute({ inspector: false }, "push")}
            me={me}
            onOpen={(entry) => open(entry)}
            onRename={(entry) => setDialog({ kind: "rename", entry })}
            onDelete={(entries) => setDialog({ kind: "delete", entries })}
            onShare={(entry) => setDialog({ kind: "share", entry })}
            onTransfer={(op, entries) => setDialog({ kind: "transfer", op, entries })}
            onTransferSelection={(op) => setDialog({ kind: "transfer", op, entries: pickedEntries })}
            // The star is stored per user and the row beside the panel draws the same bit, so the
            // panel asks for a reload rather than keeping a private copy: two components that each
            // hold their own "is this starred" disagree after exactly one click.
            onStar={() => reload()}
            onRefresh={reload}
            onSelectAll={() => setPicked(visible.map((e) => e.id))}
            onClearSelection={clearPicked}
          />
        ) : null}
      </div>

      {dragging ? (
        <div className="wc-drop">
          <div className="wc-drop__box">
            <span className="wc-drop__badge">
              <Icon name="upload" size={26} />
            </span>
            <p className="wc-drop__title">{t("files.dropHere")}</p>
            <p className="wc-drop__where">{dir ? `/${dir}` : t("files.root")}</p>
          </div>
        </div>
      ) : null}

      {moving ? (
        <div className="wc-dragghost" style={{ left: moving.x, top: moving.y }}>
          <Icon name="move" size={11} />
          {moving.entry.name}
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

      {/* The same control for a resume. Separate from the upload input so that picking a file
          to resume can never start a new upload: the two paths must not be one mis-click apart. */}
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

      {dialog?.kind === "mkdir" ? (
        <NewFolderDialog dir={dir} onClose={() => setDialog(null)} onDone={reload} />
      ) : null}
      {dialog?.kind === "rename" ? (
        <RenameDialog entry={dialog.entry} onClose={() => setDialog(null)} onDone={reload} />
      ) : null}
      {dialog?.kind === "delete" ? (
        <DeleteDialog
          entries={dialog.entries}
          onClose={() => setDialog(null)}
          onDone={() => {
            clearPicked();
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
          startDir={dir}
          onClose={() => setDialog(null)}
          onDone={() => {
            clearPicked();
            reload();
          }}
        />
      ) : null}
    </div>
  );
}

/* --------------------------------------------------------------- list view --- */

function ListView({
  rows,
  picked,
  focusId,
  listRef,
  onPick,
  onOpen,
  onKey,
  onMenu,
  onAction,
  onPointerDownRow,
  onDropOnRow,
  dragTarget,
  onSort,
  sort,
  dir,
  shares,
  t,
  fmt,
  onFilterTag,
}: {
  rows: FileNode[];
  picked: string[];
  focusId: string | null;
  listRef: React.RefObject<HTMLDivElement | null>;
  onPick: (entry: FileNode, e: React.MouseEvent | null) => void;
  onOpen: (entry: FileNode) => void;
  onKey: (e: React.KeyboardEvent) => void;
  onMenu: (entry: FileNode) => void;
  onAction: (kind: "rename" | "delete" | "share" | "copy" | "move" | "star" | "inspector", entry: FileNode) => void;
  onPointerDownRow: (
    entry: FileNode,
    e: { button: number; clientX: number; clientY: number; target: EventTarget },
  ) => void;
  onDropOnRow: (entry: FileNode) => Promise<void>;
  dragTarget: string | null;
  onSort: (key: SortKey) => void;
  sort: SortKey;
  dir: SortDir;
  shares: Share[] | null;
  /** onFilterTag narrows the whole tree to one label. Called from the row's menu, where the target
   *  is a full-size menu item rather than an 18px chip. */
  onFilterTag: (tag: { id: string; name: string }) => void;
  t: (key: string, params?: Record<string, string | number>) => string;
  fmt: { size: (bytes: number) => string; relative: (iso: string) => string };
}) {
  const labelId = "files-list-label";

  return (
    <div className="serein-filelist" role="grid" aria-labelledby={labelId} aria-multiselectable="true">
      <div className="serein-filelist__header" role="row">
        <span role="columnheader">
          <span className="serein-sr-only">{t("files.colSelect")}</span>
        </span>
        <span role="columnheader" aria-sort={sort === "name" ? (dir === "asc" ? "ascending" : "descending") : "none"}>
          <button type="button" className="serein-filelist__sort" onClick={() => onSort("name")}>
            {t("files.colName")}
            {sort === "name" ? <SortArrow dir={dir} /> : null}
          </button>
        </span>
        <span className="serein-filelist__header-where" role="columnheader">
          {t("files.colAccess")}
        </span>
        <span
          className="serein-filelist__header-modified"
          role="columnheader"
          aria-sort={sort === "modified" ? (dir === "asc" ? "ascending" : "descending") : "none"}
        >
          <button type="button" className="serein-filelist__sort" onClick={() => onSort("modified")}>
            {t("files.colModified")}
            {sort === "modified" ? <SortArrow dir={dir} /> : null}
          </button>
        </span>
        <span
          className="serein-filelist__header-size"
          role="columnheader"
          aria-sort={sort === "size" ? (dir === "asc" ? "ascending" : "descending") : "none"}
        >
          <button type="button" className="serein-filelist__sort" onClick={() => onSort("size")}>
            {t("files.colSize")}
            {sort === "size" ? <SortArrow dir={dir} /> : null}
          </button>
        </span>
        <span className="serein-filelist__header-access" role="columnheader">
          {t("files.colShare")}
        </span>
        <span className="serein-filelist__header-actions" role="columnheader">
          <span className="serein-sr-only">{t("files.colActions")}</span>
        </span>
      </div>

      <div
        ref={listRef}
        className="serein-filelist__body"
        role="rowgroup"
        tabIndex={0}
        aria-activedescendant={focusId ? `row-${focusId}` : undefined}
        aria-label={t("files.listLabel")}
        id={labelId}
        onKeyDown={onKey}
      >
        {rows.map((entry) => {
          const selected = picked.includes(entry.id);
          const isDir = entry.kind === "dir";
          const shareCount = entry.share_count ?? shares?.filter((s) => s.node_path === entry.path && !s.revoked_at).length ?? 0;
          const where = parentOf(entry.path);
          return (
            <div
              key={entry.id}
              id={`row-${entry.id}`}
              className={`serein-file-row${dragTarget === entry.path ? " wc-drop-target" : ""}`}
              role="row"
              aria-selected={selected}
              data-selected={selected}
              data-folder={isDir ? "true" : undefined}
              data-drop-folder={isDir ? entry.path : undefined}
              onMouseDown={(e) => onPointerDownRow(entry, e)}
              onClick={(e) => onPick(entry, e)}
              onDoubleClick={() => onOpen(entry)}
              onDragOver={(e) => {
                if (isDir && e.dataTransfer.types.includes("Files")) {
                  e.preventDefault();
                  e.currentTarget.classList.add("wc-drop-target");
                }
              }}
              onDragLeave={(e) => e.currentTarget.classList.remove("wc-drop-target")}
              onDrop={(e) => {
                if (!isDir) return;
                e.preventDefault();
                e.stopPropagation();
                e.currentTarget.classList.remove("wc-drop-target");
                if (e.dataTransfer.files.length > 0) return;
                void onDropOnRow(entry);
              }}
            >
              <span className="serein-file-row__check" role="gridcell">
                <label className="serein-rowcheck">
                  <input
                    type="checkbox"
                    checked={selected}
                    aria-label={t("files.selectRow", { name: entry.name })}
                    onChange={() => {
                      setPickedToggle(onPick, entry);
                    }}
                    onClick={(e) => e.stopPropagation()}
                  />
                  <span className="serein-rowcheck__box" aria-hidden="true" />
                </label>
              </span>

              <span className="serein-file-row__main" role="gridcell">
                <FileTile entry={entry} size={34} />
                <span className="serein-file-row__name">
                  <button
                    type="button"
                    className="serein-file-row__title"
                    onClick={(e) => {
                      e.stopPropagation();
                      onOpen(entry);
                    }}
                    tabIndex={-1}
                  >
                    {truncateMiddle(entry.name, 72)}
                  </button>
                  <span className="serein-file-row__sub">
                    {/*
                      A folder has no size and the listing carries no child count, so the row
                      says only "Folder". The previous "· 0 items" was an invented fact: the
                      seed folder holds children and the number came from a field that means
                      something else. The count appears where it is real — on the folder's own
                      page, in its description.
                    */}
                    <span>{kindLabel(t, entry)}</span>
                    {isDir ? null : <span> · {fmt.size(entry.size)}</span>}
                    {where ? (
                      <span className="serein-file-row__sub-where">{where}</span>
                    ) : null}
                    {/* Labels sit in the metadata line rather than beside the name: the name is the
                        one thing on a row that must not be truncated further, and a chip is a hint
                        about a file rather than part of its identity.

                        They are NOT interactive here. A chip is 18px tall, which is a third of the
                        44px touch target a phone needs (WCAG 2.5.8), and the row around it is
                        already a click target — nesting a small button inside a clickable row gives
                        a tap two possible meanings. Filtering by a label is done from the search
                        palette's picker, where the control is a full-size select. */}
                    {entry.tags && entry.tags.length > 0 ? (
                      <span className="wc-row-tags">
                        <TagChips tags={entry.tags} max={3} />
                      </span>
                    ) : null}
                  </span>
                </span>
              </span>

              <span className="serein-file-row__where" role="gridcell">
                <Icon name="files" size={11} />
                {where || t("files.root")}
              </span>

              <span className="serein-file-row__modified" role="gridcell">
                {fmt.relative(entry.modified_at)}
              </span>

              <span className="serein-file-row__size" role="gridcell">
                {isDir ? (
                  <span className="serein-file-row__dash" aria-hidden="true">
                    —
                  </span>
                ) : (
                  fmt.size(entry.size)
                )}
              </span>

              <span className="serein-file-row__access" role="gridcell">
                {shareCount > 0 ? (
                  <span className="serein-badge serein-badge--accent">
                    <Icon name="link" size={9} />
                    {shareCount}
                  </span>
                ) : (
                  <span className="serein-file-row__dash" aria-hidden="true">
                    —
                  </span>
                )}
              </span>

              <span className="serein-file-row__actions" role="gridcell">
                <details className="serein-menu" onClick={(e) => e.stopPropagation()}>
                  <summary
                    className="serein-icon-btn serein-icon-btn--sm"
                    aria-label={t("files.rowMenu", { name: entry.name })}
                    onClick={() => onMenu(entry)}
                  >
                    <Icon name="chevronDown" size={13} />
                  </summary>
                  <div className="serein-menu__surface" role="menu">
                    <button
                      type="button"
                      className="serein-menu__item"
                      role="menuitem"
                      onClick={() => onAction("inspector", entry)}
                    >
                      <span className="serein-menu__icon">
                        <Icon name="eye" size={13} />
                      </span>
                      {t("files.showInfo")}
                    </button>
                    <button
                      type="button"
                      className="serein-menu__item"
                      role="menuitem"
                      onClick={() => onOpen(entry)}
                    >
                      <span className="serein-menu__icon">
                        <Icon name={isDir ? "files" : "eye"} size={13} />
                      </span>
                      {isDir ? t("files.openFolder") : t("detail.open")}
                    </button>
                    {!isDir ? (
                      <a
                        className="serein-menu__item"
                        role="menuitem"
                        href={downloadURL(entry.path)}
                        download
                      >
                        <span className="serein-menu__icon">
                          <Icon name="download" size={13} />
                        </span>
                        {t("detail.download")}
                      </a>
                    ) : null}
                    <div className="serein-menu__sep" role="separator" />
                    {/* One item per label, so "show me everything with this label" has a target a
                        finger can hit. The list is capped: a file with nine labels would otherwise
                        push the destructive item off the menu. */}
                    {(entry.tags ?? []).slice(0, 5).map((tag) => (
                      <button
                        key={tag.id}
                        type="button"
                        className="serein-menu__item"
                        role="menuitem"
                        onClick={() => onFilterTag(tag)}
                      >
                        <span className="serein-menu__icon">
                          <Icon name="tag" size={13} />
                        </span>
                        {tag.name}
                      </button>
                    ))}
                    {(entry.tags ?? []).length > 0 ? (
                      <div className="serein-menu__sep" role="separator" />
                    ) : null}
                    <button
                      type="button"
                      className="serein-menu__item"
                      role="menuitem"
                      onClick={() => onAction("star", entry)}
                    >
                      <span className="serein-menu__icon">
                        <Icon name={entry.starred ? "starFill" : "star"} size={13} />
                      </span>
                      {/* The label states the ACTION, not the state: a menu item called "Starred"
                          next to a checked box reads as an assertion, and a menu item called
                          "Remove star" reads as the thing the click will do. */}
                      {entry.starred ? t("social.unstar") : t("social.star")}
                    </button>
                    <div className="serein-menu__sep" role="separator" />
                    <button
                      type="button"
                      className="serein-menu__item"
                      role="menuitem"
                      onClick={() => onAction("share", entry)}
                    >
                      <span className="serein-menu__icon">
                        <Icon name="link" size={13} />
                      </span>
                      {t("detail.share")}
                    </button>
                    <button
                      type="button"
                      className="serein-menu__item"
                      role="menuitem"
                      onClick={() => onAction("rename", entry)}
                    >
                      <span className="serein-menu__icon">
                        <Icon name="edit" size={13} />
                      </span>
                      {t("detail.rename")}
                    </button>
                    <button
                      type="button"
                      className="serein-menu__item"
                      role="menuitem"
                      onClick={() => onAction("move", entry)}
                    >
                      <span className="serein-menu__icon">
                        <Icon name="move" size={13} />
                      </span>
                      {t("detail.move")}
                    </button>
                    <button
                      type="button"
                      className="serein-menu__item"
                      role="menuitem"
                      onClick={() => onAction("copy", entry)}
                    >
                      <span className="serein-menu__icon">
                        <Icon name="copy" size={13} />
                      </span>
                      {t("detail.copy")}
                    </button>
                    <div className="serein-menu__sep" role="separator" />
                    <button
                      type="button"
                      className="serein-menu__item serein-menu__item--danger"
                      role="menuitem"
                      onClick={() => onAction("delete", entry)}
                    >
                      <span className="serein-menu__icon">
                        <Icon name="trash" size={13} />
                      </span>
                      {t("detail.delete")}
                    </button>
                  </div>
                </details>
              </span>
            </div>
          );
        })}
      </div>
    </div>
  );
}

/**
 * setPickedToggle is the checkbox's own semantics: it toggles one row without clearing the
 * rest, which is what a checkbox means. Reusing the row's click handler would make a
 * checkbox behave like a plain click and silently drop a multi-selection.
 */
function setPickedToggle(
  onPick: (entry: FileNode, e: React.MouseEvent | null) => void,
  entry: FileNode,
): void {
  // A synthetic ctrl-click reproduces "toggle this row" exactly, including the anchor, so the
  // checkbox and the modifier click cannot diverge in behaviour as the selection code grows.
  const fake = { shiftKey: false, metaKey: true, ctrlKey: false } as React.MouseEvent;
  onPick(entry, fake);
}

function SortArrow({ dir }: { dir: SortDir }) {
  return (
    <span aria-hidden="true" style={{ display: "inline-flex", rotate: dir === "asc" ? "180deg" : "0deg" }}>
      <Icon name="chevronDown" size={10} />
    </span>
  );
}

function parentOf(path: string): string {
  const clean = path.replace(/^\//, "");
  const i = clean.lastIndexOf("/");
  return i < 0 ? "" : clean.slice(0, i);
}

/**
 * truncateMiddle keeps both the beginning and the extension of a long name.
 *
 * A name truncated at the end hides the extension, which is the part that says what the file
 * is. CSS `text-overflow` cannot do that, so the string is shortened before it is laid out and
 * the full name stays in the title attribute and the accessible name.
 */
export function truncateMiddle(name: string, max: number): string {
  if (name.length <= max) return name;
  const ext = extensionOf(name);
  const keep = Math.floor((max - ext.length - 2) / 2);
  if (keep < 4) return name.slice(0, max - 1) + "…";
  return `${name.slice(0, keep)}…${name.slice(-(keep + ext.length + 1))}`;
}

/* --------------------------------------------------------------- grid view --- */

function GridView({
  rows,
  picked,
  focusId,
  onPick,
  onOpen,
}: {
  rows: FileNode[];
  picked: string[];
  focusId: string | null;
  onPick: (entry: FileNode, e: React.MouseEvent | null) => void;
  onOpen: (entry: FileNode) => void;
}) {
  return (
    <div className="serein-file-grid" role="listbox" aria-multiselectable="true">
      {rows.map((entry) => {
        const selected = picked.includes(entry.id);
        return (
          <div
            key={entry.id}
            className="serein-file-grid__item"
            role="option"
            id={`row-${entry.id}`}
            aria-selected={selected}
            data-selected={selected}
            tabIndex={focusId === entry.id ? 0 : -1}
          >
            <button
              type="button"
              className="serein-file-tile serein-file-tile--interactive"
              onClick={(e) => onPick(entry, e)}
              onDoubleClick={() => onOpen(entry)}
            >
              <span className="serein-file-tile__preview">
                <FileTile entry={entry} size={220} />
              </span>
              <span className="serein-file-tile__name">{entry.name}</span>
              <span className="serein-file-tile__meta">
                <span>{kindLabelFrom(entry)}</span>
              </span>
            </button>
          </div>
        );
      })}
    </div>
  );
}

function kindLabelFrom(entry: FileNode): string {
  const ext = extensionOf(entry.name);
  if (entry.kind === "dir") return "";
  if (ext) return ext.toUpperCase();
  return "";
}

/* ---------------------------------------------------------------- helpers --- */

function isDenied(message: string): boolean {
  const lower = message.toLowerCase();
  return lower.includes("not_found") || lower.includes("denied") || lower.includes("forbidden");
}

/* Types re-exported for the shell's convenience; the route owns the vocabulary. */
export type { KindFilter, SortKey, SortDir, ViewMode };
