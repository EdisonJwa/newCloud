// Shared interface primitives.
//
// These are the pieces every surface needs and none of them owns: the icon set, the toast
// channel, the three states a list can be in, and the file-kind vocabulary. Serein UI supplies
// the visual components; what lives here is the *product* interpretation of our data — which
// icon a preview kind gets, what a file type is called, which tile colour it takes.
//
// There is no generic component library here on purpose. A Button that takes twelve props to
// serve one caller is an abstraction paid for and never used, and Serein already ships that
// layer properly. What this file adds is the mapping from `FileNode` to those classes.

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { useI18n } from "./i18n";
import { thumbnailURL } from "./api";
import type { FileNode, PreviewKind } from "./types";

/* ------------------------------------------------------------------ icons --- */

/**
 * The icon set, in the 16px grid the shell uses.
 *
 * Filled paths rather than a sprite: the whole set is under 4 kB, it needs no request, and a
 * missing icon degrades to a visible placeholder instead of a blank square. Icons are always
 * decorative here — every one of them sits next to a text label or inside a control that
 * carries its own `aria-label`.
 */
const PATHS: Record<string, string> = {
  search: "M7.25 12.5a5.25 5.25 0 1 0 0-10.5 5.25 5.25 0 0 0 0 10.5Zm3.8-.55 3.7 3.7",
  home: "M2.75 7.6 8 3.1l5.25 4.5M4.2 6.7V13h7.6V6.7",
  files:
    "M3 4.4A1.4 1.4 0 0 1 4.4 3h2.5l1.2 1.5H13a1 1 0 0 1 1 1V12a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V4.4Z",
  shares:
    "M5.4 7.9a1.7 1.7 0 1 1-3.4 0 1.7 1.7 0 0 1 3.4 0Zm8.6-4a1.7 1.7 0 1 1-3.4 0 1.7 1.7 0 0 1 3.4 0Zm0 7.2a1.7 1.7 0 1 1-3.4 0 1.7 1.7 0 0 1 3.4 0ZM5.7 7.4l5.2-2M5.7 8.4l5.2 2.1",
  trash:
    "M3.4 5h9.2M6.1 5V3.9a.9.9 0 0 1 .9-.9h2a.9.9 0 0 1 .9.9V5M5 5l.4 8a.9.9 0 0 0 .9.85h3.4A.9.9 0 0 0 10.6 13L11 5",
  key: "M9.4 6.6a2.5 2.5 0 1 1-1.8 4.2L4.4 14H2.9v-1.5l3.3-3.2a2.5 2.5 0 0 1 3.2-2.7Z",
  pulse: "M1.6 8h2.8l1.5-4.1 2.4 8.2 1.6-4.1h3.5",
  rows: "M2.6 4h10.8M2.6 8h10.8M2.6 12h10.8",
  grid: "M2.6 2.6h4.3v4.3H2.6zM9.1 2.6h4.3v4.3H9.1zM2.6 9.1h4.3v4.3H2.6zM9.1 9.1h4.3v4.3H9.1z",
  copy: "M5.6 5.6h7.2v7.2H5.6zM2.6 10.6V2.6h8",
  move: "M2.6 8h7.3m0 0L7.4 5.5m2.5 2.5L7.4 10.5M12 3v10h1.5",
  upload: "M8 11V3m0 0L5 6m3-3 3 3M2.5 11.6V13a1 1 0 0 0 1 1h9a1 1 0 0 0 1-1v-1.4",
  folderPlus:
    "M3 4.4A1.4 1.4 0 0 1 4.4 3h2.5l1.2 1.5H13a1 1 0 0 1 1 1V12a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1V4.4ZM8 7.4v3.4M6.3 9.1h3.4",
  refresh: "M13.4 8a5.4 5.4 0 1 1-1.6-3.85M13.4 2v3h-3",
  download: "M8 2.5v8m0 0 3-3m-3 3-3-3M2.5 13.5h11",
  close: "M3.9 3.9l8.2 8.2M12.1 3.9l-8.2 8.2",
  doc: "M4 2.5h5l3 3v8H4zM9 2.5v3h3",
  user: "M8 8.2a2.6 2.6 0 1 0 0-5.2 2.6 2.6 0 0 0 0 5.2ZM3.4 13.6a4.6 4.6 0 0 1 9.2 0",
  chevron: "M6 3.2l4 4.8-4 4.8",
  chevronDown: "M3.2 6l4.8 4 4.8-4",
  arrowUp: "M8 13V3m0 0L4 7m4-4 4 4",
  arrowLeft: "M13 8H3m0 0 4-4M3 8l4 4",
  command: "M6 3.6A1.6 1.6 0 1 0 4.4 5.2h7.2A1.6 1.6 0 1 0 10 3.6v8.8a1.6 1.6 0 1 0 1.6-1.6H4.4A1.6 1.6 0 1 0 6 12.4z",
  layers: "M8 2.1 14 5l-6 2.9L2 5l6-2.9ZM2 8l6 2.9L14 8M2 11l6 2.9L14 11",
  alert: "M8 2.6 14 13.4H2L8 2.6ZM8 6.4v3.1M8 11.4v.6",
  check: "M3 8.4 6.4 12 13 4.6",
  offline: "M2 2l12 12M5 8a4 4 0 0 1 5.6-.5M2.6 5.6A8 8 0 0 1 13 6M8 12v.5",
  eye: "M1.5 8s2.4-4 6.5-4 6.5 4 6.5 4-2.4 4-6.5 4S1.5 8 1.5 8Zm6.5 2.1a2.1 2.1 0 1 0 0-4.2 2.1 2.1 0 0 0 0 4.2Z",
  link: "M6.5 9.5 9.5 6.5M5.6 11a2.6 2.6 0 0 1-3.7-3.7L4 5.2a2.6 2.6 0 0 1 3.7 0M10.4 5a2.6 2.6 0 0 1 3.7 3.7L12 10.8a2.6 2.6 0 0 1-3.7 0",
  inbox:
    "M2.4 9.4 4 3.4h8l1.6 6v3.4h-11.2zM2.4 9.4h3.1l1 1.5h3l1-1.5h3.1",
  edit: "M2.5 13.5 3.2 10.7l7-7 2.1 2.1-7 7-2.8.7ZM10 3.8l2.2 2.2",
  star: "M8 2.4l1.8 3.7 4 .6-2.9 2.9.7 4-3.6-1.9-3.6 1.9.7-4L2.2 6.7l4-.6L8 2.4Z",
  starFill: "M8 2.4l1.8 3.7 4 .6-2.9 2.9.7 4-3.6-1.9-3.6 1.9.7-4L2.2 6.7l4-.6L8 2.4Z",
  sidebar: "M2.6 3.4h10.8v9.2H2.6zM6.4 3.4v9.2",
  settings:
    "M8 10.1a2.1 2.1 0 1 0 0-4.2 2.1 2.1 0 0 0 0 4.2Zm4.9-2.1c0-.3 0-.6-.1-.9l1.5-1-1.4-2.4-1.7.7a3.9 3.9 0 0 0-1.6-.9l-.3-1.8H6.9l-.3 1.8a3.9 3.9 0 0 0-1.6.9l-1.7-.7-1.4 2.4 1.5 1a5 5 0 0 0 0 1.8l-1.5 1 1.4 2.4 1.7-.7c.5.4 1 .7 1.6.9l.3 1.8h2.6l.3-1.8c.6-.2 1.1-.5 1.6-.9l1.7.7 1.4-2.4-1.5-1c.1-.3.1-.6.1-.9Z",
  palette:
    "M8 2.4a5.6 5.6 0 0 0 0 11.2c1 0 1.5-.6 1.5-1.3 0-.8-.7-1.1-.7-1.8 0-.6.5-1 1.1-1h.9a3.6 3.6 0 0 0 3.6-3.6C14.4 3.9 11.5 2.4 8 2.4Zm-2.9 4.6a.9.9 0 1 1 0-1.8.9.9 0 0 1 0 1.8Zm2.8-1.2a.9.9 0 1 1 0-1.8.9.9 0 0 1 0 1.8Zm2.9 1.2a.9.9 0 1 1 0-1.8.9.9 0 0 1 0 1.8Z",
  keyboard:
    "M2.4 5.2h11.2v5.6H2.4zM4.6 7.4h.01M6.9 7.4h.01M9.2 7.4h.01M11.4 7.4h.01M5.4 9.4h5.2",
  storage:
    "M2.4 4.4h11.2v3.2H2.4zM2.4 8.4h11.2v3.2H2.4zM4.6 6h.01M4.6 10h.01",
  logout: "M6 2.6H3.4v10.8H6M10.2 8H6.4m3.8 0L8.2 6M10.2 8 8.2 10",
  plus: "M8 3v10M3 8h10",
  minus: "M3 8h10",
  image: "M2.4 3.4h11.2v9.2H2.4zM5.6 6.4a.9.9 0 1 1-1.8 0 .9.9 0 0 1 1.8 0Zm-.4 5 3.2-3.4 2.2 2.4 1.4-1.4 2 2.4",
  play: "M6 4.4l5.2 3.6L6 11.6z",
  music: "M6.4 10.6V4.4l5.2-1.2v6.2M6.4 10.6a1.4 1.4 0 1 1-2.8 0 1.4 1.4 0 0 1 2.8 0Zm5.2-1.2a1.4 1.4 0 1 1-2.8 0 1.4 1.4 0 0 1 2.8 0Z",
  code: "M5.6 5 2.6 8l3 3M10.4 5l3 3-3 3",
  sheet: "M2.6 3.4h10.8v9.2H2.6zM2.6 6.5h10.8M2.6 9.5h10.8M6.5 3.4v9.2",
  archive: "M3 3.4h10v9.2H3zM6.5 3.4v2.2h3V3.4M8 6.6v2.6m0 2.4v.4",
  sheetFile: "M4 2.5h5l3 3v8H4zM6 7.5h4M6 9.5h4M6 11.5h2.5",
  design: "M3.4 3.4h6.2v6.2H3.4zM6.4 6.4h6.2v6.2H6.4z",
  download2: "M8 2.5v8m0 0 3-3m-3 3-3-3M2.5 13.5h11",
  // Core-scope §5 and §6: a label and a discussion. Drawn in the same 16px grid and with the same
  // stroke weight as the rest of the set, because an icon that is visually louder than its
  // neighbours reads as a warning rather than as a category.
  tag: "M2.6 3.4h5.3l5.5 5.5-5.3 5.3-5.5-5.5zM5.6 6.4h.01",
  comment: "M2.6 3.4h10.8v7H8.2L5 13.4V10.4H2.6z",
};

export function Icon({ name, size = 15 }: { name: string; size?: number }) {
  const path = PATHS[name] ?? PATHS.files;
  const filled = name === "starFill";
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 16 16"
      fill={filled ? "currentColor" : "none"}
      stroke="currentColor"
      strokeWidth={1.25}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
      focusable="false"
    >
      <path d={path} />
    </svg>
  );
}

/* ----------------------------------------------------------------- toasts --- */

export interface ToastAction {
  label: string;
  run: () => void;
}

interface ToastItem {
  id: number;
  message: string;
  tone: "neutral" | "success" | "warning" | "danger";
  action?: ToastAction;
}

interface ToastAPI {
  info: (message: string, action?: ToastAction) => void;
  success: (message: string, action?: ToastAction) => void;
  warning: (message: string) => void;
  error: (message: string, action?: ToastAction) => void;
}

const NOOP_TOAST: ToastAPI = {
  info: () => {},
  success: () => {},
  warning: () => {},
  error: () => {},
};

const ToastContext = createContext<ToastAPI>(NOOP_TOAST);

export function useToast(): ToastAPI {
  return useContext(ToastContext);
}

/**
 * ToastProvider owns the visible queue and the live region.
 *
 * Every toast is announced as well as shown, because a visual-only confirmation is a silent
 * failure for a screen reader user. The region is `aria-live="polite"` and always present: a
 * region that materialises with its text is frequently not announced at all.
 *
 * The queue is capped at three. A failed batch upload that produces twenty errors would
 * otherwise cover the interface that the person is trying to use to fix it.
 */
export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([]);
  const [announcement, setAnnouncement] = useState("");
  const nextId = useRef(1);
  const timers = useRef<number[]>([]);

  const push = useCallback(
    (message: string, tone: ToastItem["tone"], action?: ToastAction) => {
      const id = nextId.current++;
      setItems((prev) => [...prev.slice(-2), { id, message, tone, action }]);
      setAnnouncement(message);
      // A danger message stays longer: it usually asks the person to do something.
      const timer = window.setTimeout(
        () => setItems((prev) => prev.filter((t) => t.id !== id)),
        tone === "danger" ? 8000 : 3600,
      );
      timers.current.push(timer);
    },
    [],
  );

  const api = useMemo<ToastAPI>(
    () => ({
      info: (message, action) => push(message, "neutral", action),
      success: (message, action) => push(message, "success", action),
      warning: (message) => push(message, "warning"),
      error: (message, action) => push(message, "danger", action),
    }),
    [push],
  );

  useEffect(
    () => () => {
      for (const t of timers.current) window.clearTimeout(t);
    },
    [],
  );

  return (
    <ToastContext.Provider value={api}>
      {children}
      <div className="wc-live" role="status" aria-live="polite" aria-atomic="true">
        {announcement}
      </div>
      <div className="serein-toast-region" role="region" aria-label="">
        {items.map((item) => (
          <div key={item.id} className={`serein-toast serein-toast--${item.tone}`} role="status">
            <span className="serein-toast__message">{item.message}</span>
            {item.action ? (
              <button
                type="button"
                className="serein-toast__action"
                onClick={() => {
                  setItems((prev) => prev.filter((t) => t.id !== item.id));
                  item.action?.run();
                }}
              >
                {item.action.label}
              </button>
            ) : null}
            <button
              type="button"
              className="serein-toast__close"
              aria-label="Dismiss"
              onClick={() => setItems((prev) => prev.filter((t) => t.id !== item.id))}
            >
              <Icon name="close" size={11} />
            </button>
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}

/* -------------------------------------------------------- announcements --- */

interface AnnounceAPI {
  /** Announces a message to assistive technology without showing it. */
  announce: (message: string) => void;
}

const AnnounceContext = createContext<AnnounceAPI>({ announce: () => {} });

export function useAnnounce(): AnnounceAPI {
  return useContext(AnnounceContext);
}

/**
 * AnnounceProvider is separate from the toast channel on purpose.
 *
 * Stepping through a filmstrip changes the file 40 times in a minute; forty toasts would be
 * an unusable interface, but silence would mean a keyboard user cannot tell which file they
 * are on. This announces the same facts at the same moments without drawing anything.
 */
export function AnnounceProvider({ children }: { children: ReactNode }) {
  const [message, setMessage] = useState("");
  // Clearing first forces a change event even when the same text is announced twice in a row
  // (stepping forward and back onto the same file). Without it the region's content is
  // identical and nothing is spoken.
  const announce = useCallback((next: string) => {
    setMessage("");
    window.setTimeout(() => setMessage(next), 30);
  }, []);
  const value = useMemo(() => ({ announce }), [announce]);

  return (
    <AnnounceContext.Provider value={value}>
      {children}
      <div className="wc-live" role="status" aria-live="polite" aria-atomic="true">
        {message}
      </div>
    </AnnounceContext.Provider>
  );
}

/* ----------------------------------------------------------- file kind --- */

/**
 * The extension groups that override the server's `preview` kind.
 *
 * `preview: "text"` is true of a `.md` note, a `.json` config and a `.csv` export alike, but
 * they are three different things to look at. The extension is the more specific fact, so it is
 * consulted first for the formats whose tile would otherwise be wrong.
 */
const SHEET_EXT = ["csv", "tsv", "xls", "xlsx", "ods", "numbers"];
const DESIGN_EXT = ["psd", "sketch", "fig", "ai", "xd", "indd", "afdesign"];
const ARCHIVE_EXT = ["zip", "gz", "tar", "7z", "rar", "bz2", "xz", "zst"];

/** The Serein file-icon modifier for a node. */
export function kindClass(entry: Pick<FileNode, "kind" | "preview" | "name">): string {
  if (entry.kind === "dir") return "folder";
  const ext = extensionOf(entry.name).toLowerCase();
  // Specific formats first: a CSV is previewed as text but is a spreadsheet.
  if (ARCHIVE_EXT.includes(ext)) return "archive";
  if (SHEET_EXT.includes(ext)) return "sheet";
  if (DESIGN_EXT.includes(ext)) return "design";
  switch (entry.preview) {
    case "image":
      return "image";
    case "pdf":
      return "pdf";
    case "video":
      return "video";
    case "audio":
      return "audio";
    case "text":
      // Both prose and source use Serein's code tile: the alternative is the neutral fill,
      // which reads as "unrecognised type" next to a `.md` file. The icon differs.
      return "code";
    default:
      return "default";
  }
}

/** The icon inside a file tile. Always paired with a visible type label or a filename. */
export function kindIcon(entry: Pick<FileNode, "kind" | "preview" | "name">): string {
  if (entry.kind === "dir") return "files";
  const ext = extensionOf(entry.name).toLowerCase();
  if (ARCHIVE_EXT.includes(ext)) return "archive";
  if (SHEET_EXT.includes(ext)) return "sheet";
  if (DESIGN_EXT.includes(ext)) return "design";
  switch (entry.preview) {
    case "image":
      return "image";
    case "pdf":
      return "doc";
    case "video":
      return "play";
    case "audio":
      return "music";
    case "text":
      return looksLikeCode(entry.name) ? "code" : "doc";
    default:
      return "doc";
  }
}

export function extensionOf(name: string): string {
  const idx = name.lastIndexOf(".");
  if (idx <= 0 || idx === name.length - 1) return "";
  return name.slice(idx + 1);
}

/**
 * looksLikeCode reports whether a text file is source rather than prose.
 *
 * The distinction matters in the viewer, where the two get different layouts, and in the
 * resume preview diagram. It is an extension list rather than a content sniff because the
 * server already told us `preview: "text"` and guessing further from the first 200 bytes is
 * how a JSON data file gets syntax-highlighted as Python.
 */
export function looksLikeCode(name: string): boolean {
  const ext = extensionOf(name).toLowerCase();
  return [
    "js",
    "jsx",
    "ts",
    "tsx",
    "mjs",
    "cjs",
    "py",
    "go",
    "rs",
    "rb",
    "java",
    "kt",
    "c",
    "h",
    "cc",
    "cpp",
    "cs",
    "php",
    "swift",
    "sh",
    "bash",
    "zsh",
    "ps1",
    "sql",
    "json",
    "yaml",
    "yml",
    "toml",
    "xml",
    "html",
    "css",
    "scss",
    "vue",
    "svelte",
  ].includes(ext);
}

export function isImage(entry: Pick<FileNode, "preview">): boolean {
  return entry.preview === "image";
}

/**
 * FileTile renders the small content-adaptive diagram used in the resume band, recent rows
 * and the grid.
 *
 * A photograph gets its own thumbnail. Everything else gets a truthful little drawing of the
 * format rather than one generic glyph, which is the difference between "some document" and
 * "that spreadsheet". The drawing is CSS and inline SVG so it scales with Large Text and costs
 * no request.
 */
export function FileTile({
  entry,
  size = 34,
  className,
}: {
  entry: Pick<FileNode, "kind" | "preview" | "name" | "path">;
  size?: number;
  className?: string;
}) {
  const [broken, setBroken] = useState(false);
  const kind = kindClass(entry);
  const ext = extensionOf(entry.name);

  if (entry.kind === "file" && isImage(entry) && !broken) {
    return (
      <span
        className={`serein-file-icon serein-file-icon--thumb${className ? ` ${className}` : ""}`}
        style={{ ["--serein-file-icon-size" as string]: `${size}px` }}
        aria-hidden="true"
      >
        <img
          src={(entry as FileNode).path ? thumbnailURL((entry as FileNode).path) : ""}
          alt=""
          loading="lazy"
          onError={() => setBroken(true)}
        />
      </span>
    );
  }

  return (
    <span
      className={`serein-file-icon serein-file-icon--${kind}${className ? ` ${className}` : ""}`}
      style={{ ["--serein-file-icon-size" as string]: `${size}px` }}
      aria-hidden="true"
    >
      <Icon name={kindIcon(entry)} size={Math.round(size * 0.48)} />
      {ext && size >= 40 ? <span className="serein-file-icon__format">{ext.slice(0, 4)}</span> : null}
    </span>
  );
}

/* ----------------------------------------------------------- view states --- */

export function EmptyState({
  icon = "files",
  title,
  hint,
  actions,
  error = false,
}: {
  icon?: string;
  title: string;
  hint?: string;
  actions?: ReactNode;
  error?: boolean;
}) {
  return (
    <div className={`serein-empty${error ? " serein-empty--error" : ""}`}>
      <span className="serein-empty__icon">
        <Icon name={icon} size={22} />
      </span>
      <p className="serein-empty__title">{title}</p>
      {hint ? <p className="serein-empty__description">{hint}</p> : null}
      {actions ? <div className="serein-empty__actions">{actions}</div> : null}
    </div>
  );
}

export function Skeleton({ rows = 7 }: { rows?: number }) {
  return (
    <div className="wc-skeleton-rows" aria-hidden="true">
      {Array.from({ length: rows }, (_, i) => (
        <div className="wc-skeleton-row" key={i}>
          <span className="serein-skeleton" style={{ width: 34, height: 34 }} />
          <span className="wc-skeleton-row__stack">
            <span className="serein-skeleton serein-skeleton--text" style={{ width: `${52 + ((i * 13) % 34)}%` }} />
            <span className="serein-skeleton serein-skeleton--text" style={{ width: `${26 + ((i * 7) % 18)}%` }} />
          </span>
        </div>
      ))}
    </div>
  );
}

export function ErrorState({
  message,
  onRetry,
  title,
}: {
  message: string;
  onRetry: () => void;
  title?: string;
}) {
  const { t } = useI18n();
  return (
    <EmptyState
      error
      icon="alert"
      title={title ?? t("state.errorTitle")}
      hint={message}
      actions={
        <button type="button" className="serein-btn" onClick={onRetry}>
          {t("state.retry")}
        </button>
      }
    />
  );
}

export function DeniedState({ onHome }: { onHome: () => void }) {
  const { t } = useI18n();
  return (
    <EmptyState
      icon="alert"
      title={t("state.deniedTitle")}
      hint={t("state.deniedHint")}
      actions={
        <button type="button" className="serein-btn serein-btn--outline" onClick={onHome}>
          {t("nav.home")}
        </button>
      }
    />
  );
}

/* --------------------------------------------------------------- labels --- */

/**
 * useLabel returns a stable id for tying a control to its description.
 *
 * React's `useId` output contains colons, which are legal in an `id` but not in a CSS selector,
 * so anything that needs to query one later would need escaping. Prefixing and stripping keeps
 * both uses available.
 */
export function useLabel(prefix: string): (suffix: string) => string {
  const base = useId().replace(/[^a-zA-Z0-9]/g, "");
  return useCallback((suffix: string) => `${prefix}-${base}-${suffix}`, [base, prefix]);
}

/**
 * formatCountLabel picks the plural form and the number in one call.
 *
 * A single helper so a caller cannot accidentally write `n + " files"`, which is untranslatable.
 */
export function countLabel(
  t: (key: string, params?: Record<string, string | number>) => string,
  key: string,
  n: number,
): string {
  return t(key, { n });
}

/**
 * previewingAsText describes a preview kind without the word "preview".
 *
 * Used where a column is too narrow to say "Preview" and where the reader already knows what
 * the row is. Keeping it in one place means the vocabulary in a row subtitle, an inspector and
 * a share card cannot drift.
 */
export function kindLabel(
  t: (key: string, params?: Record<string, string | number>) => string,
  entry: Pick<FileNode, "kind" | "preview" | "name">,
): string {
  if (entry.kind === "dir") return t("kind.folder");
  const ext = extensionOf(entry.name);
  const lower = ext.toLowerCase();
  // Extension first where it is more specific than the preview kind: a `.csv` is previewed as
  // text but is a spreadsheet, and calling it "Text" is the sort of small wrongness that makes
  // a file manager feel like it does not know what it is holding.
  if (SHEET_EXT.includes(lower)) return t("kind.sheet");
  if (DESIGN_EXT.includes(lower)) return t("kind.design");
  if (ARCHIVE_EXT.includes(lower)) return t("kind.archive");
  switch (entry.preview) {
    case "image":
      return t("kind.image");
    case "video":
      return t("kind.video");
    case "audio":
      return t("kind.audio");
    case "pdf":
      return t("kind.pdf");
    case "text":
      return looksLikeCode(entry.name) ? t("kind.code") : t("kind.text");
    default:
      break;
  }
  if (ext) return ext.toUpperCase();
  return t("kind.file");
}

/**
 * previewLabel is the same idea for a raw `PreviewKind`, which is what the server sends and
 * what `kindLabel` derives from when a node has one.
 */
export function previewLabel(
  t: (key: string) => string,
  preview: PreviewKind | undefined,
): string {
  if (!preview) return t("kind.file");
  return t(`kind.${preview}`);
}
