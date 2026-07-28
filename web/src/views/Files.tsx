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
  useRef,
  useState,
  type DragEvent,
  type KeyboardEvent,
} from "react";
import { api, APIError, downloadURL, inlineURL, thumbnailURL } from "../api";
import { useI18n } from "../i18n";
import { DeniedState, EmptyState, ErrorState, FileGlyph, Icon, Skeleton, useToast } from "../ui";
import { DeleteDialog, NewFolderDialog, RenameDialog, ShareDialog } from "../components/Dialogs";
import { Viewer } from "../components/Viewer";
import type { FileNode, Share, Version } from "../types";

interface Props {
  initialPath?: string;
  /** Bumped by the shell when something outside this view changed the tree. */
  reloadKey: number;
  onPathChange: (path: string) => void;
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
  const [viewer, setViewer] = useState<{ entries: FileNode[]; index: number } | null>(null);
  const [dialog, setDialog] = useState<
    | { kind: "mkdir" }
    | { kind: "rename"; entry: FileNode }
    | { kind: "delete"; entries: FileNode[] }
    | { kind: "share"; entry: FileNode }
    | null
  >(null);
  const [upload, setUpload] = useState<{ done: number; total: number } | null>(null);
  const [dragging, setDragging] = useState(false);

  const colRefs = useRef<Array<HTMLDivElement | null>>([]);
  const wantFocus = useRef(false);
  const fileInput = useRef<HTMLInputElement>(null);
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
  }, []);

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
      setUpload({ done: 0, total: files.length });
      let done = 0;
      let failed = 0;
      for (const file of files) {
        try {
          await api.upload(deepest, file);
          done += 1;
        } catch {
          failed += 1;
        }
        setUpload({ done: done + failed, total: files.length });
      }
      setUpload(null);
      reload();
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

  /* ---------- keyboard ---------- */

  const onColumnKey = (e: KeyboardEvent<HTMLDivElement>, level: number) => {
    const dir = dirs[level] ?? "";
    const entries = filtered(listings[dir] ?? [], level === dirs.length - 1 ? filter : "");
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
        if (sel) {
          e.preventDefault();
          setDialog({ kind: "delete", entries: [sel.entry] });
        }
        return;
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
          <span className="mono upload-count">
            {upload.done}/{upload.total}
          </span>
        ) : null}
        <span className="spacer" />
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
            const entries = filtered(listings[dir] ?? [], level === dirs.length - 1 ? filter : "");
            const isLast = level === dirs.length - 1;
            const loadingHere = loading[dir] && listings[dir] === undefined;

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
                <div className="colsub" aria-hidden="true">
                  <span>{t("files.name")}</span>
                  <span>{t("files.size")}</span>
                </div>

                <div
                  ref={(node) => {
                    colRefs.current[level] = node;
                  }}
                  className="colbody"
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

                  {entries.map((entry) => {
                    const selected = sel?.level === level && sel.entry.id === entry.id;
                    return (
                      <div
                        key={entry.id}
                        id={`row-${level}-${entry.id}`}
                        role="option"
                        aria-selected={selected}
                        className={`row${entry.kind === "dir" ? " dir" : ""}`}
                        onClick={() => select(level, entry)}
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

          {sel ? (
            <Detail
              key={sel.entry.id}
              entry={sel.entry}
              onClose={() => setSel(null)}
              onOpen={() => open(sel.level, sel.entry)}
              onRename={() => setDialog({ kind: "rename", entry: sel.entry })}
              onDelete={() => setDialog({ kind: "delete", entries: [sel.entry] })}
              onShare={() => setDialog({ kind: "share", entry: sel.entry })}
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
    </>
  );
}

/* ---------- detail ---------- */

interface DetailProps {
  entry: FileNode;
  onClose: () => void;
  onOpen: () => void;
  onRename: () => void;
  onDelete: () => void;
  onShare: () => void;
  onRefresh: () => void;
}

function Detail({ entry, onClose, onOpen, onRename, onDelete, onShare, onRefresh }: DetailProps) {
  const { t, fmt } = useI18n();
  const toast = useToast();
  const [versions, setVersions] = useState<Version[] | null>(null);
  const [shares, setShares] = useState<Share[] | null>(null);

  useEffect(() => {
    let live = true;
    if (entry.has_versions) {
      api
        .versions(entry.path)
        .then((res) => live && setVersions(res.versions ?? []))
        .catch(() => live && setVersions([]));
    } else {
      setVersions([]);
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
              versions.map((v) => (
                <div className="vrow" key={v.version}>
                  <span className="no">v{v.version}</span>
                  <span className="meta">
                    {fmt.size(v.size)} · {fmt.relative(v.created_at)}
                  </span>
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
                </div>
              ))
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

function filtered(entries: FileNode[], filter: string): FileNode[] {
  const needle = filter.trim().toLowerCase();
  if (!needle) return entries;
  return entries.filter((e) => e.name.toLowerCase().includes(needle));
}

function isDenied(message: string): boolean {
  const lower = message.toLowerCase();
  return lower.includes("not_found") || lower.includes("forbidden") || lower.includes("404");
}

