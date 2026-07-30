// Shared UI primitives.
//
// Small on purpose: an icon set, a toast channel, and the three states every view needs.
// There is no generic component library here because there is no second consumer — a Button
// that takes twelve props to serve one caller is an abstraction paid for and never used.

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { useI18n } from "./i18n";
import type { FileNode } from "./types";

/* ---------- icons ---------- */

const PATHS: Record<string, string> = {
  search: "M10.5 10.5l3.5 3.5M7.5 12.5a5 5 0 100-10 5 5 0 000 10z",
  home: "M2.5 7.5L8 2.5l5.5 5M4 6.5V13h8V6.5",
  files: "M3 4.5A1.5 1.5 0 014.5 3h2.4l1.2 1.5H13A1 1 0 0114 5.5v7.5a1 1 0 01-1 1H4a1 1 0 01-1-1V4.5z",
  shares:
    "M5.5 8a1.5 1.5 0 11-3 0 1.5 1.5 0 013 0zM13.5 4.5a1.5 1.5 0 11-3 0 1.5 1.5 0 013 0zM13.5 11.5a1.5 1.5 0 11-3 0 1.5 1.5 0 013 0zM5.8 7.4l5-1.9M5.8 8.6l5 1.9",
  trash: "M3.5 5h9M6 5V3.8a.8.8 0 01.8-.8h2.4a.8.8 0 01.8.8V5M5 5l.4 8a.9.9 0 00.9.9h3.4a.9.9 0 00.9-.9L11 5",
  key: "M9.5 6.5a2.5 2.5 0 11-1.7 4.3L4.5 14H3v-1.5l3.8-3.3a2.5 2.5 0 012.7-2.7z",
  pulse: "M1.5 8h3l1.5-4 2.5 8 1.5-4h3.5",
  rows: "M2.5 4h11M2.5 8h11M2.5 12h11",
  grid: "M2.5 2.5h4.5v4.5H2.5zM9 2.5h4.5v4.5H9zM2.5 9h4.5v4.5H2.5zM9 9h4.5v4.5H9z",
  upload: "M8 11V3m0 0L5 6m3-3l3 3M2.5 11.5v1.5a1 1 0 001 1h9a1 1 0 001-1v-1.5",
  folderPlus:
    "M3 4.5A1.5 1.5 0 014.5 3h2.4l1.2 1.5H13A1 1 0 0114 5.5v7.5a1 1 0 01-1 1H4a1 1 0 01-1-1V4.5zM8 7.5v3.5M6.25 9.25h3.5",
  refresh: "M13.5 8a5.5 5.5 0 11-1.6-3.9M13.5 2v3h-3",
  download: "M8 2.5v8m0 0l3-3m-3 3l-3-3M2.5 13.5h11",
  close: "M4 4l8 8M12 4l-8 8",
  doc: "M4 2.5h5l3 3v8H4zM9 2.5v3h3",
  user: "M8 8a2.5 2.5 0 100-5 2.5 2.5 0 000 5zm-4.5 5.5a4.5 4.5 0 019 0",
  chevron: "M6 3l4 5-4 5",
  arrowUp: "M8 13V3m0 0L4 7m4-4l4 4",
  command: "M6 3.5a1.5 1.5 0 11-1.5 1.5v6a1.5 1.5 0 11-1.5-1.5h10a1.5 1.5 0 11-1.5 1.5v-6A1.5 1.5 0 0112 5a1.5 1.5 0 111.5-1.5",
  layers: "M8 2l6 3-6 3-6-3 6-3zM2 8l6 3 6-3M2 11l6 3 6-3",
  alert: "M8 2.5l6 11H2l6-11zM8 6.5v3M8 11.5v.5",
  offline: "M2 2l12 12M5 8a4 4 0 015.5-.5M2.5 5.5A8 8 0 0113 6M8 12v.5",
  eye: "M1.5 8s2.4-4 6.5-4 6.5 4 6.5 4-2.4 4-6.5 4S1.5 8 1.5 8zM8 10a2 2 0 100-4 2 2 0 000 4z",
  link: "M6.5 9.5l3-3M5.5 11a2.5 2.5 0 01-3.5-3.5l2-2a2.5 2.5 0 013.5 0M10.5 5a2.5 2.5 0 013.5 3.5l-2 2a2.5 2.5 0 01-3.5 0",
  inbox: "M2.5 9.5l1.6-6h7.8l1.6 6v3.5h-11zM2.5 9.5h3l1 1.5h3l1-1.5h3",
  edit: "M2.5 13.5l.7-2.8 7-7 2.1 2.1-7 7-2.8.7zM10 3.8l2.2 2.2",
};

export function Icon({ name, size = 15 }: { name: string; size?: number }) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 16 16"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.3}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d={PATHS[name] ?? PATHS.files} />
    </svg>
  );
}

/* ---------- toasts ---------- */

export interface ToastAction {
  label: string;
  run: () => void;
}

interface ToastItem {
  id: number;
  message: string;
  kind: "info" | "error";
  action?: ToastAction;
}

interface ToastAPI {
  info: (message: string, action?: ToastAction) => void;
  error: (message: string) => void;
}

const ToastContext = createContext<ToastAPI>({ info: () => {}, error: () => {} });

export function useToast(): ToastAPI {
  return useContext(ToastContext);
}

// ToastProvider also owns the polite live region. Every toast is announced there as well as
// shown, because a visual-only confirmation is a silent failure for a screen reader user.
export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([]);
  const [announcement, setAnnouncement] = useState("");
  const nextId = useRef(1);
  const timers = useRef<number[]>([]);

  const push = useCallback((message: string, kind: "info" | "error", action?: ToastAction) => {
    const id = nextId.current++;
    setItems((prev) => [...prev.slice(-2), { id, message, kind, action }]);
    setAnnouncement(message);
    const timer = window.setTimeout(
      () => setItems((prev) => prev.filter((t) => t.id !== id)),
      kind === "error" ? 6000 : 3200,
    );
    timers.current.push(timer);
  }, []);

  const api = useMemo<ToastAPI>(
    () => ({
      info: (message, action) => push(message, "info", action),
      error: (message) => push(message, "error"),
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
      <div className="sr-only" role="status" aria-live="polite" aria-atomic="true">
        {announcement}
      </div>
      {items.map((item) => (
        <div key={item.id} className={`toast${item.kind === "error" ? " error" : ""}`} role="status">
          <span>{item.message}</span>
          {item.action ? (
            <button
              type="button"
              onClick={() => {
                setItems((prev) => prev.filter((t) => t.id !== item.id));
                item.action?.run();
              }}
            >
              {item.action.label}
            </button>
          ) : null}
        </div>
      ))}
    </ToastContext.Provider>
  );
}

/* ---------- file glyph ---------- */

export function typeClass(entry: FileNode): string {
  if (entry.kind === "dir") return "dir";
  switch (entry.preview) {
    case "image":
      return "img";
    case "video":
      return "vid";
    case "audio":
      return "aud";
    case "pdf":
      return "doc";
    case "text":
      return "txt";
  }
  return "";
}

function extensionOf(name: string): string {
  const idx = name.lastIndexOf(".");
  if (idx <= 0 || idx === name.length - 1) return "";
  return name.slice(idx + 1);
}

export function FileGlyph({ entry, thumb }: { entry: FileNode; thumb?: string }) {
  const { t } = useI18n();
  const [broken, setBroken] = useState(false);

  if (thumb && entry.preview === "image" && !broken) {
    return (
      <span className="glyph img">
        <img src={thumb} alt="" loading="lazy" onError={() => setBroken(true)} />
      </span>
    );
  }

  const label =
    entry.kind === "dir"
      ? ""
      : entry.preview
        ? t(`kind.${entry.preview}`).slice(0, 3)
        : (extensionOf(entry.name) || "?").slice(0, 3);

  return (
    <span className={`glyph ${typeClass(entry)}`} aria-hidden="true">
      {entry.kind === "dir" ? <Icon name="files" size={12} /> : label}
    </span>
  );
}

/* ---------- view states ---------- */

export function EmptyState({
  icon = "files",
  title,
  hint,
  actions,
}: {
  icon?: string;
  title: string;
  hint?: string;
  actions?: ReactNode;
}) {
  return (
    <div className="state">
      <div className="glyph">
        <Icon name={icon} size={18} />
      </div>
      <div className="t">{title}</div>
      {hint ? <div className="h"> {hint}</div> : null}
      {actions ? <div className="acts">{actions}</div> : null}
    </div>
  );
}

export function Skeleton({ rows = 7 }: { rows?: number }) {
  return (
    <div className="skeleton" aria-hidden="true">
      {Array.from({ length: rows }, (_, i) => (
        <i key={i} />
      ))}
    </div>
  );
}

export function ErrorState({ message, onRetry }: { message: string; onRetry: () => void }) {
  const { t } = useI18n();
  return (
    <EmptyState
      icon="alert"
      title={t("state.errorTitle")}
      hint={message}
      actions={
        <button type="button" className="btn primary" onClick={onRetry}>
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
        <button type="button" className="btn" onClick={onHome}>
          {t("nav.home")}
        </button>
      }
    />
  );
}
