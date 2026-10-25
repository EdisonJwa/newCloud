// The File Viewer.
//
// One viewer, reached by Space, Enter, a double click, a tap, the row menu and a direct URL,
// and every one of those is a browser-history entry with a URL. The previous product had a
// temporary overlay and a separate screen, so two renderers and two navigation models had to
// be kept in step; they drifted, and the overlay drew a generic icon for anything that was not
// a photograph.
//
// ## The collection
//
// The viewer is scoped to the collection you came from, and that collection is explicit and in
// the URL (`from=`). Opening a file in Northstar Campaign and pressing Next walks that
// folder's files, not the whole workspace, and at a boundary the button is disabled rather
// than wrapping into unrelated content. Closing returns to the originating folder with its
// filter, sort and selection intact, because those live in the route rather than in the viewer.
//
// ## What each format actually gets
//
//   image    <img>, with zoom, pan and double-click to fit
//   video    <video controls>
//   audio    <audio controls> with its own artwork
//   PDF      an inline frame; the browser's own viewer is better than a hand-rolled one
//   text     a fetched, line-numbered block; source gets monospace and tabular columns
//   CSV/TSV  a real table, because a spreadsheet of 40 rows is readable and useful
//   XLSX/ODS an honest fallback card ??decoding a binary workbook in the browser is a parser
//            and a dependency for one preview, and the prototype's fabricated sheet would be
//            dishonest to ship
//   other    a fallback card with the name, type, size and a working Download
//
// The footer changes with the size of the collection: a single file gets a compact strip with
// both directions disabled, two to three files get Previous / position / Next, and four or
// more get the scrollable filmstrip. A one-item filmstrip is a control that does nothing.
//
// ## Received shares are not rendered inline
//
// `/api/shared/{id}/content` answers `application/octet-stream` with an attachment disposition,
// by design: it is the copy-taking route, not a rendering route, and the content origin is
// attachment-only. So a file somebody else owns cannot be put in an `<img>` or an `<iframe>`
// here. The viewer says so and offers the two things that do work ??the server's own
// thumbnail for an image, and Download. Pretending otherwise would produce a broken picture.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { api, APIError, downloadURL, inlineURL, sharedContentURL, thumbnailURL } from "../api";
import { useI18n } from "../i18n";
import {
  Icon,
  FileTile,
  kindLabel,
  useAnnounce,
  useToast,
  extensionOf,
  looksLikeCode,
} from "../ui";
import type { FileNode } from "../types";
import type { Route } from "../route";

interface Props {
  route: Route;
  collection: CollectionState;
  onIndex: (index: number) => void;
  onClose: () => void;
  onRetry: () => void;
  onToggleInspector: () => void;
  inspectorOpen: boolean;
}

const ZOOM_MIN = 1;
const ZOOM_MAX = 4;
const ZOOM_STEP = 0.25;

/** The character cap for an inline text render, stated in the caption where it applies. */
const TEXT_CAP = 400_000;

export function Viewer(props: Props) {
  const { t, fmt } = useI18n();
  const toast = useToast();
  const { announce } = useAnnounce();
  const closeRef = useRef<HTMLButtonElement>(null);
  const [zoom, setZoom] = useState(1);
  const [pan, setPan] = useState({ x: 0, y: 0 });
  const [chrome, setChrome] = useState(true);

  const entries = props.collection.entries ?? [];
  const index = Math.min(Math.max(0, props.route.at), Math.max(0, entries.length - 1));
  const entry = entries[index] ?? null;
  const canPrev = index > 0;
  const canNext = index < entries.length - 1;
  const isImage = entry?.preview === "image";

  const step = useCallback(
    (delta: number) => {
      const next = index + delta;
      if (next < 0 || next >= entries.length) return;
      const target = entries[next];
      if (!target) return;
      setZoom(1);
      setPan({ x: 0, y: 0 });
      props.onIndex(next);
      announce(t("viewer.position", { n: next + 1, total: entries.length, name: target.name }));
    },
    [index, entries, props, announce, t],
  );

  // Escape closes; arrows step; +/- zoom; 0 fits; F toggles chrome. All on the window rather
  // than the dialog so a click into the canvas cannot disable them, with a guard for a field
  // that legitimately wants the arrow keys.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const target = e.target as HTMLElement | null;
      if (target && target.closest("input, textarea, select, [contenteditable='true']")) return;
      switch (e.key) {
        case "Escape":
          e.preventDefault();
          props.onClose();
          return;
        case "ArrowLeft":
          if (e.altKey) return;
          e.preventDefault();
          step(-1);
          return;
        case "ArrowRight":
          if (e.altKey) return;
          e.preventDefault();
          step(1);
          return;
        case "+":
        case "=":
          e.preventDefault();
          setZoom((z) => Math.min(ZOOM_MAX, z + ZOOM_STEP));
          return;
        case "-":
          e.preventDefault();
          setZoom((z) => Math.max(ZOOM_MIN, z - ZOOM_STEP));
          return;
        case "0":
          e.preventDefault();
          setZoom(1);
          setPan({ x: 0, y: 0 });
          return;
        case "i":
          if (e.metaKey || e.ctrlKey) {
            e.preventDefault();
            props.onToggleInspector();
          }
          return;
        case "f":
          if (!e.metaKey && !e.ctrlKey && !e.altKey) setChrome((v) => !v);
          return;
        default:
          break;
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [props, step]);

  useEffect(() => {
    // Focus lands on Close rather than the body: the viewer replaces the page, so a keyboard
    // user needs one predictable place to start from that also announces where they are.
    closeRef.current?.focus();
  }, []);

  useEffect(() => {
    setZoom(1);
    setPan({ x: 0, y: 0 });
  }, [entry?.id]);

  if (props.collection.loading) {
    return (
      <ViewerFrame>
        <div className="serein-viewer" aria-busy="true" aria-label={t("viewer.loading")}>
          <div className="serein-viewer__bar">
            <div className="serein-viewer__lead">
              <span className="serein-skeleton serein-skeleton--title" style={{ width: 190 }} />
            </div>
          </div>
          <div className="serein-viewer__stage">
            <div className="serein-viewer__main">
              <div className="serein-viewer__canvas">
                <span className="serein-spinner" aria-hidden="true" />
              </div>
            </div>
          </div>
        </div>
      </ViewerFrame>
    );
  }

  if (props.collection.error) {
    return (
      <ViewerFrame>
        <div className="serein-viewer" role="dialog" aria-modal="true" aria-label={t("viewer.label")}>
          <div className="serein-viewer__bar">
            <div className="serein-viewer__lead">
              <button
                ref={closeRef}
                type="button"
                className="serein-btn serein-btn--sm serein-btn--quiet"
                onClick={props.onClose}
              >
                <Icon name="arrowLeft" size={13} />
                {t("viewer.back")}
              </button>
            </div>
          </div>
          <div className="serein-viewer__stage">
            <div className="serein-viewer__main">
              <div className="serein-viewer__canvas">
                <div className="serein-viewer__fallback">
                  <span className="serein-viewer__fallback-icon">
                    <Icon name="alert" size={22} />
                  </span>
                  <p className="serein-viewer__fallback-title">{t("viewer.unavailableTitle")}</p>
                  <p className="serein-viewer__fallback-text">{props.collection.error}</p>
                  <div className="serein-viewer__fallback-actions">
                    <button type="button" className="serein-btn serein-btn--sm" onClick={props.onRetry}>
                      {t("state.retry")}
                    </button>
                    <button
                      type="button"
                      className="serein-btn serein-btn--sm serein-btn--quiet"
                      onClick={props.onClose}
                    >
                      {t("viewer.back")}
                    </button>
                  </div>
                </div>
              </div>
            </div>
          </div>
        </div>
      </ViewerFrame>
    );
  }

  if (!entry) {
    return (
      <ViewerFrame>
        <div className="serein-viewer" role="dialog" aria-modal="true" aria-label={t("viewer.label")}>
          <div className="serein-viewer__stage">
            <div className="serein-viewer__main">
              <div className="serein-viewer__canvas">
                <div className="serein-viewer__fallback">
                  <span className="serein-viewer__fallback-icon">
                    <Icon name="files" size={22} />
                  </span>
                  <p className="serein-viewer__fallback-title">{t("viewer.goneTitle")}</p>
                  <p className="serein-viewer__fallback-text">{t("viewer.goneText")}</p>
                  <div className="serein-viewer__fallback-actions">
                    <button type="button" className="serein-btn serein-btn--sm" onClick={props.onClose}>
                      {t("viewer.back")}
                    </button>
                  </div>
                </div>
              </div>
            </div>
          </div>
        </div>
      </ViewerFrame>
    );
  }

  const download = props.collection.shared ? sharedContentURL(entry.path) : downloadURL(entry.path);

  return (
    <ViewerFrame>
      <div
        className="serein-viewer"
        role="dialog"
        aria-modal="true"
        aria-label={t("viewer.label", { name: entry.name })}
      >
        {chrome ? (
          <div className="serein-viewer__bar">
            <div className="serein-viewer__lead">
              <button
                ref={closeRef}
                type="button"
                className="serein-icon-btn"
                aria-label={t("viewer.close")}
                title={t("viewer.close")}
                onClick={props.onClose}
              >
                <Icon name="close" size={15} />
              </button>
              <div className="serein-viewer__title">
                <span className="serein-viewer__name">{entry.name}</span>
                <span className="serein-viewer__meta">
                  {kindLabel(t, entry)} · {fmt.size(entry.size)} · {fmt.relative(entry.modified_at)}
                </span>
              </div>
            </div>

            <div className="serein-viewer__tools">
              {isImage && !props.collection.shared ? (
                <>
                  <div className="serein-viewer__zoom" role="group" aria-label={t("viewer.zoom")}>
                    <button
                      type="button"
                      className="serein-icon-btn"
                      aria-label={t("viewer.zoomOut")}
                      disabled={zoom <= ZOOM_MIN}
                      onClick={() => setZoom((z) => Math.max(ZOOM_MIN, z - ZOOM_STEP))}
                    >
                      <Icon name="minus" size={13} />
                    </button>
                    <span className="serein-viewer__zoom-value" aria-live="polite">
                      {Math.round(zoom * 100)}%
                    </span>
                    <button
                      type="button"
                      className="serein-icon-btn"
                      aria-label={t("viewer.zoomIn")}
                      disabled={zoom >= ZOOM_MAX}
                      onClick={() => setZoom((z) => Math.min(ZOOM_MAX, z + ZOOM_STEP))}
                    >
                      <Icon name="plus" size={13} />
                    </button>
                  </div>
                  {zoom > ZOOM_MIN ? (
                    <button
                      type="button"
                      className="serein-btn serein-btn--sm serein-btn--quiet"
                      onClick={() => {
                        setZoom(1);
                        setPan({ x: 0, y: 0 });
                      }}
                    >
                      {t("viewer.fit")}
                    </button>
                  ) : null}
                  <span className="serein-viewer__sep" aria-hidden="true" />
                </>
              ) : null}
              <button
                type="button"
                className="serein-icon-btn"
                aria-pressed={props.inspectorOpen}
                aria-label={t("inspector.toggle")}
                title={t("inspector.toggle")}
                onClick={props.onToggleInspector}
              >
                <Icon name="sidebar" size={15} />
              </button>
              <a
                className="serein-icon-btn"
                href={download}
                download
                aria-label={t("preview.download")}
                title={t("preview.download")}
              >
                <Icon name="download" size={15} />
              </a>
            </div>
          </div>
        ) : null}

        <div className="serein-viewer__stage">
          <div className="serein-viewer__main">
            <div
              className="serein-viewer__canvas"
              data-layout={
                isImage
                  ? "media"
                  : entry.preview === "pdf" || entry.preview === "text"
                    ? "document"
                    : "compact"
              }
              onClick={() => {
                // Clicking the image is the way most people dismiss the chrome to see the whole
                // photograph. It is not offered on a document, where a click is a text selection.
                if (isImage && zoom === 1) setChrome((v) => !v);
              }}
            >
              <Stage
                entry={entry}
                shared={props.collection.shared}
                zoom={zoom}
                pan={pan}
                onPan={setPan}
                onZoom={(next) => setZoom(Math.min(ZOOM_MAX, Math.max(ZOOM_MIN, next)))}
                onDownload={() => {
                  window.location.href = download;
                }}
                onError={() => toast.error(t("preview.mediaFailed"))}
              />
            </div>

            <div
              className={`serein-viewer__footer${entries.length <= 1 ? " serein-viewer__footer--compact" : ""}`}
            >
              <div className="serein-viewer__footer-meta">
                <strong>{props.collection.label}</strong>
                <span>{props.collection.location}</span>
              </div>

              {entries.length > 3 ? (
                <div className="serein-viewer__filmstrip" role="tablist" aria-label={t("viewer.filmstrip")}>
                  {entries.map((item, i) => (
                    <button
                      key={item.id}
                      type="button"
                      role="tab"
                      className="serein-viewer__film"
                      aria-selected={i === index}
                      aria-current={i === index ? "true" : undefined}
                      aria-label={item.name}
                      tabIndex={i === index ? 0 : -1}
                      onClick={() => props.onIndex(i)}
                    >
                      <FilmTile entry={item} shared={props.collection.shared} />
                    </button>
                  ))}
                </div>
              ) : (
                // No filmstrip below four items ??and no second copy of the position either:
                // the nav below already says "2 / 3", and printing it twice in one footer was
                // visible in the captured CSV screenshot.
                <span />
              )}

              <div className="serein-viewer__nav">
                <button
                  type="button"
                  className="serein-viewer__step"
                  disabled={!canPrev}
                  aria-label={t("preview.prev")}
                  onClick={() => step(-1)}
                >
                  <Icon name="arrowLeft" size={13} />
                  <span className="wc-step-label">{t("preview.prev")}</span>
                </button>
                <span className="serein-viewer__count">
                  <strong>{index + 1}</strong> / {entries.length}
                </span>
                <button
                  type="button"
                  className="serein-viewer__step"
                  disabled={!canNext}
                  aria-label={t("preview.next")}
                  onClick={() => step(1)}
                >
                  <span className="wc-step-label">{t("preview.next")}</span>
                  <Icon name="chevron" size={13} />
                </button>
              </div>
            </div>
          </div>
        </div>
      </div>
    </ViewerFrame>
  );
}

function ViewerFrame({ children }: { children: React.ReactNode }) {
  return <div className="wc-viewer-host">{children}</div>;
}

/* ----------------------------------------------------------------- stage --- */

function Stage({
  entry,
  shared,
  zoom,
  pan,
  onPan,
  onZoom,
  onDownload,
  onError,
}: {
  entry: FileNode;
  shared: boolean;
  zoom: number;
  pan: { x: number; y: number };
  onPan: (next: { x: number; y: number }) => void;
  onZoom: (next: number) => void;
  onDownload: () => void;
  onError: () => void;
}) {
  const { t, fmt } = useI18n();
  const [text, setText] = useState<string | null>(null);
  const [textError, setTextError] = useState(false);
  const [failed, setFailed] = useState(false);
  const [dragging, setDragging] = useState(false);
  const dragStart = useRef<{ x: number; y: number; px: number; py: number } | null>(null);
  const isSpreadsheet = ["csv", "tsv"].includes(extensionOf(entry.name).toLowerCase());

  useEffect(() => {
    setText(null);
    setTextError(false);
    setFailed(false);
    // A received file's bytes are attachment-only, so there is nothing to fetch for a text
    // render. Asking anyway would produce a 200 carrying an octet-stream.
    if (shared) return;
    const needsBody = entry.preview === "text" || isSpreadsheet;
    if (!needsBody) return;

    let live = true;
    fetch(inlineURL(entry.path), { credentials: "same-origin" })
      .then((r) => (r.ok ? r.text() : Promise.reject(new Error(String(r.status)))))
      .then((body) => {
        // Only a prefix is shown: a 200 MB log would lock the tab, and the point of the
        // preview is to recognise the file, not to read it in full.
        if (live) setText(body.slice(0, TEXT_CAP));
      })
      .catch(() => {
        if (live) setTextError(true);
      });
    return () => {
      live = false;
    };
  }, [entry.path, entry.preview, isSpreadsheet, shared]);

  if (shared) {
    return (
      <SharedStage entry={entry} onDownload={onDownload} />
    );
  }

  if (failed || textError) {
    return (
      <Fallback
        entry={entry}
        title={t("preview.unavailable")}
        text={t("preview.unavailableHint")}
        onDownload={onDownload}
      />
    );
  }

  if (entry.preview === "image") {
    return (
      <img
        className="serein-viewer__image"
        src={inlineURL(entry.path)}
        alt={entry.name}
        draggable={false}
        style={{
          // Translate before scale so the pan is in screen pixels rather than scaled ones.
          transform: `translate(${pan.x}px, ${pan.y}px) scale(${zoom})`,
          cursor: zoom > 1 ? (dragging ? "grabbing" : "grab") : "default",
          transition: dragging ? "none" : "transform var(--serein-duration-2) var(--serein-ease-out)",
        }}
        onError={() => setFailed(true)}
        onWheel={(e) => {
          // Wheel-zoom only with a modifier: an unmodified wheel over a full-screen image is
          // a scroll gesture, and hijacking it is the classic image-viewer annoyance.
          if (!e.ctrlKey && !e.metaKey) return;
          e.preventDefault();
          onZoom(zoom + (e.deltaY < 0 ? ZOOM_STEP : -ZOOM_STEP));
        }}
        onPointerDown={(e) => {
          if (zoom <= 1) return;
          (e.target as HTMLElement).setPointerCapture?.(e.pointerId);
          dragStart.current = { x: e.clientX, y: e.clientY, px: pan.x, py: pan.y };
          setDragging(true);
        }}
        onPointerMove={(e) => {
          const start = dragStart.current;
          if (!start) return;
          onPan({ x: start.px + (e.clientX - start.x), y: start.py + (e.clientY - start.y) });
        }}
        onPointerUp={() => {
          dragStart.current = null;
          setDragging(false);
        }}
        onPointerCancel={() => {
          dragStart.current = null;
          setDragging(false);
        }}
        onDoubleClick={() => onZoom(zoom > 1 ? 1 : 2)}
      />
    );
  }

  if (entry.preview === "video") {
    return <video src={inlineURL(entry.path)} controls playsInline onError={onError} />;
  }

  if (entry.preview === "audio") {
    return (
      <div className="wc-audio">
        <span className="wc-audio__art" aria-hidden="true">
          <Icon name="music" size={34} />
        </span>
        <p className="wc-audio__name">{entry.name}</p>
        <p className="wc-audio__meta">
          {kindLabel(t, entry)} · {fmt.size(entry.size)}
        </p>
        <audio src={inlineURL(entry.path)} controls onError={onError} />
      </div>
    );
  }

  if (entry.preview === "pdf") {
    return <iframe className="wc-pdf" src={inlineURL(entry.path)} title={entry.name} />;
  }

  if (isSpreadsheet) {
    if (text === null) {
      return (
        <p className="wc-stage-loading">
          <span className="serein-spinner" aria-hidden="true" />
          {t("state.loading")}
        </p>
      );
    }
    return <Sheet name={entry.name} text={text} />;
  }

  if (entry.preview === "text") {
    if (text === null) {
      return (
        <p className="wc-stage-loading">
          <span className="serein-spinner" aria-hidden="true" />
          {t("state.loading")}
        </p>
      );
    }
    const code = looksLikeCode(entry.name);
    return (
      <figure className="wc-text">
        <pre className={code ? "wc-text__pre wc-text__pre--code" : "wc-text__pre"}>
          <code>{text}</code>
        </pre>
        <figcaption className="wc-text__caption">
          {text.length >= TEXT_CAP
            ? t("viewer.textCapTruncated", { shown: TEXT_CAP.toLocaleString() })
            : t("viewer.textCap")}
        </figcaption>
      </figure>
    );
  }

  return (
    <Fallback
      entry={entry}
      title={t("preview.noneTitle")}
      text={t("preview.none", { kind: kindLabel(t, entry) })}
      onDownload={onDownload}
    />
  );
}

/**
 * SharedStage is what a received file can honestly show.
 *
 * The content route is attachment-only, so the two things that work are the server's own
 * thumbnail (for an image) and Download. Both are labelled: the picture is called a thumbnail
 * because it is one, and the card says why there is no full render rather than leaving an
 * empty box that reads as a broken product.
 */
function SharedStage({ entry, onDownload }: { entry: FileNode; onDownload: () => void }) {
  const { t, fmt } = useI18n();
  const [broken, setBroken] = useState(false);
  const thumb = `/api/files/thumbnail?id=${encodeURIComponent(entry.path)}`;

  return (
    <div className="serein-viewer__fallback">
      <span className="serein-viewer__fallback-icon">
        {entry.preview === "image" && !broken ? (
          <img
            className="wc-shared-thumb"
            src={thumb}
            alt={t("viewer.sharedThumbnail", { name: entry.name })}
            onError={() => setBroken(true)}
          />
        ) : (
          <FileTile entry={entry} size={48} />
        )}
      </span>
      <p className="serein-viewer__fallback-title">{entry.name}</p>
      <p className="serein-viewer__fallback-text">{t("viewer.sharedNoInline")}</p>
      <p className="serein-viewer__fallback-text wc-fallback-meta">
        {kindLabel(t, entry)} · {fmt.size(entry.size)}
        {entry.preview === "image" && !broken ? ` · ${t("viewer.sharedThumbCaption")}` : ""}
      </p>
      <div className="serein-viewer__fallback-actions">
        <button type="button" className="serein-btn serein-btn--sm" onClick={onDownload}>
          <Icon name="download" size={12} />
          {t("preview.download")}
        </button>
      </div>
    </div>
  );
}

/**
 * Fallback is the honest card for a type the viewer cannot render.
 *
 * Its job is to be useful: the name, the type, the size, and a Download that works. A blank
 * canvas with "no preview" wastes the trip; a button pretending to render would be worse.
 */
function Fallback({
  entry,
  title,
  text,
  onDownload,
}: {
  entry: FileNode;
  title: string;
  text: string;
  onDownload: () => void;
}) {
  const { t, fmt } = useI18n();
  return (
    <div className="serein-viewer__fallback">
      <span className="serein-viewer__fallback-icon">
        <FileTile entry={entry} size={48} />
      </span>
      <p className="serein-viewer__fallback-title">{title}</p>
      <p className="serein-viewer__fallback-text">{text}</p>
      <p className="serein-viewer__fallback-text wc-fallback-meta">
        {entry.name} · {kindLabel(t, entry)} · {fmt.size(entry.size)}
      </p>
      <div className="serein-viewer__fallback-actions">
        <button type="button" className="serein-btn serein-btn--sm" onClick={onDownload}>
          <Icon name="download" size={12} />
          {t("preview.download")}
        </button>
      </div>
    </div>
  );
}

function FilmTile({ entry, shared }: { entry: FileNode; shared: boolean }) {
  if (shared) {
    // No inline thumbnail route for a received node on the filmstrip path, so the type tile is
    // used: a filmstrip of identical grey squares would be worse than no picture at all.
    return <FileTile entry={entry} size={48} />;
  }
  return <FileTile entry={entry} size={48} />;
}

/**
 * Sheet renders a CSV or TSV as a real table.
 *
 * A quoted-field parser is ~50 lines and the payoff is large: a spreadsheet of 40 rows is one
 * of the most common things people share, and showing it as raw commas is the kind of
 * "technically a preview" that makes a product feel unfinished. The delimiter is guessed from
 * the first line so a semicolon export from a European locale still lines up.
 */
function Sheet({ name, text }: { name: string; text: string }) {
  const { t } = useI18n();
  const rows = useMemo(() => parseDelimited(text, name).slice(0, 200), [text, name]);
  const maxCols = useMemo(
    () => Math.min(30, rows.reduce((n, r) => Math.max(n, r.length), 0)),
    [rows],
  );
  const truncatedRows = parseDelimited(text, name).length > 200;
  const truncatedText = text.length >= TEXT_CAP;

  if (rows.length === 0) {
    return <p className="serein-hint">{t("viewer.emptySheet")}</p>;
  }

  return (
    <div className="wc-sheet">
      <div className="wc-sheet__scroll">
        <table className="serein-table serein-table--compact">
          <caption className="serein-table__caption">
            {t("viewer.sheetCap", { rows: rows.length, cols: maxCols })}
          </caption>
          <tbody>
            {rows.map((row, i) => (
              <tr key={i}>
                <th scope="row" className="wc-sheet__num">
                  {i + 1}
                </th>
                {Array.from({ length: maxCols }, (_, c) => (
                  <td key={c} className={i === 0 ? "wc-sheet__head" : undefined}>
                    {row[c] ?? ""}
                  </td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {truncatedRows || truncatedText ? (
        <p className="serein-hint wc-sheet__note">{t("viewer.truncated")}</p>
      ) : null}
    </div>
  );
}

function parseDelimited(text: string, name: string): string[][] {
  const delimiter = extensionOf(name).toLowerCase() === "tsv" ? "\t" : guessDelimiter(text);
  const rows: string[][] = [];
  let row: string[] = [];
  let field = "";
  let quoted = false;

  for (let i = 0; i < text.length; i++) {
    const ch = text[i];
    if (quoted) {
      if (ch === '"') {
        if (text[i + 1] === '"') {
          field += '"';
          i++;
        } else {
          quoted = false;
        }
      } else {
        field += ch;
      }
      continue;
    }
    if (ch === '"' && field === "") {
      quoted = true;
      continue;
    }
    if (ch === delimiter) {
      row.push(field);
      field = "";
      continue;
    }
    if (ch === "\n") {
      row.push(field.replace(/\r$/, ""));
      rows.push(row);
      row = [];
      field = "";
      continue;
    }
    field += ch;
  }
  if (field !== "" || row.length > 0) {
    row.push(field);
    rows.push(row);
  }
  return rows.filter((r) => r.length > 1 || (r[0] ?? "") !== "");
}

/** guessDelimiter picks the separator the first line actually uses most. */
function guessDelimiter(text: string): string {
  const line = text.slice(0, 2000).split("\n")[0] ?? "";
  let best = ",";
  let bestCount = 0;
  for (const c of [",", ";", "\t", "|"]) {
    const n = line.split(c).length - 1;
    if (n > bestCount) {
      best = c;
      bestCount = n;
    }
  }
  return best;
}

/* --------------------------------------------------------- collection data --- */

export interface CollectionState {
  entries: FileNode[] | null;
  loading: boolean;
  error: string | null;
  label: string;
  location: string;
  /** True when the files live in somebody else's namespace and are addressed by id. */
  shared: boolean;
}

const EMPTY_COLLECTION: CollectionState = {
  entries: null,
  loading: true,
  error: null,
  label: "",
  location: "",
  shared: false,
};

/**
 * useCollection resolves the viewer's ordered file list from its `from` identity.
 *
 * Every kind produces a *collection*, not a single file plus guesses, which is what makes Next
 * and Previous meaningful. A `single` collection that cannot list its parent falls back to the
 * file alone rather than to an empty filmstrip beside a rendered picture.
 */
export function useCollection(route: Route, enabled = true): CollectionState {
  const { t } = useI18n();
  const [state, setState] = useState<CollectionState>(EMPTY_COLLECTION);

  // The identity, not the object: `route.from` is recreated on every parse, so depending on it
  // would refetch the collection on every unrelated route change (a sort, a selection, the
  // inspector toggle) and make the viewer flicker back to its skeleton.
  const fromKey = JSON.stringify(route.from);

  useEffect(() => {
    // Outside the viewer there is no collection to build. Fetching one anyway would put a
    // recent-files request behind every navigation.
    if (!enabled) return;
    const from = JSON.parse(fromKey) as Route["from"];
    let live = true;
    setState((s) => ({ ...s, loading: true, error: null }));

    const fail = (err: unknown) => {
      if (!live) return;
      setState({
        ...EMPTY_COLLECTION,
        loading: false,
        error: err instanceof APIError ? err.message || err.code : String(err),
      });
    };

    const load = async () => {
      if (from.kind === "search") {
        const res = await api.search({ q: from.q, limit: 60 });
        if (!live) return;
        const items = (res.items ?? []).filter((f) => f.kind === "file");
        setState({
          entries: items,
          loading: false,
          error: null,
          label: t("viewer.fromSearch", { q: from.q }),
          location: t("viewer.countFiles", { n: items.length }),
          shared: false,
        });
        return;
      }

      if (from.kind === "recent") {
        const res = await api.recent(60);
        if (!live) return;
        const items = (res.items ?? []).filter((f) => f.kind === "file");
        setState({
          entries: items,
          loading: false,
          error: null,
          label: t("viewer.fromRecent"),
          location: t("viewer.countFiles", { n: items.length }),
          shared: false,
        });
        return;
      }

      if (from.kind === "shared") {
        const res = await api.sharedWithMe();
        if (!live) return;
        // A received node is addressed by id, not by path: `path` carries the id for these
        // rows so that Download and the thumbnail route can name them, and `shared` tells the
        // stage that inline rendering is not available.
        const items: FileNode[] = (res.items ?? [])
          .filter((n) => n.kind === "file")
          .map((n) => ({
            id: n.id,
            name: n.name,
            path: n.id,
            kind: "file" as const,
            size: n.size ?? 0,
            mime: n.mime ?? "",
            modified_at: n.modified_at ?? new Date().toISOString(),
            created_at: n.modified_at ?? new Date().toISOString(),
            preview: previewOf(n.mime, n.name),
          }));
        setState({
          entries: items,
          loading: false,
          error: null,
          label: t("viewer.fromShared"),
          location: t("viewer.countFiles", { n: items.length }),
          shared: true,
        });
        return;
      }

      // folder and single both resolve a real directory.
      const path = from.kind === "folder" ? from.path : parentOf(from.path);
      const res = await api.list(path);
      if (!live) return;
      const items = (res.children ?? []).filter((f) => f.kind === "file");

      if (from.kind === "single" && !items.some((f) => f.path === from.path)) {
        // The file exists (the route named it) but the listing did not include it, which
        // happens when the folder is filtered or the file sits elsewhere. One item is the
        // honest collection: a filmstrip of a file that is not in it would be worse.
        const alone: FileNode = {
          id: from.path,
          name: leafOf(from.path),
          path: from.path,
          kind: "file",
          size: 0,
          modified_at: new Date().toISOString(),
          created_at: new Date().toISOString(),
        };
        setState({
          entries: [alone],
          loading: false,
          error: null,
          label: alone.name,
          location: "",
          shared: false,
        });
        return;
      }

      setState({
        entries: items,
        loading: false,
        error: null,
        label: path ? leafOf(path) : t("files.allTitle"),
        location: t("viewer.countFiles", { n: items.length }),
        shared: false,
      });
    };

    void load().catch(fail);
    return () => {
      live = false;
    };
  }, [fromKey, enabled, t]);

  return state;
}

function parentOf(path: string): string {
  const clean = path.replace(/^\//, "");
  const i = clean.lastIndexOf("/");
  return i < 0 ? "" : clean.slice(0, i);
}

function leafOf(path: string): string {
  return path.slice(path.lastIndexOf("/") + 1) || path;
}

/**
 * previewOf infers a preview kind from a MIME type.
 *
 * The server sends `preview` on its own listings, but a received node's payload carries only
 * the MIME type. Rather than a second endpoint, the mapping mirrors the server's, and an
 * unrecognised type falls back to the honest card instead of guessing an image.
 */
function previewOf(mime: string | undefined, name: string): FileNode["preview"] {
  const type = (mime ?? "").toLowerCase();
  const ext = extensionOf(name).toLowerCase();
  if (type.startsWith("image/")) return "image";
  if (type.startsWith("video/")) return "video";
  if (type.startsWith("audio/")) return "audio";
  if (type === "application/pdf") return "pdf";
  if (type.startsWith("text/") || ["md", "txt", "log", "json", "yaml", "yml", "csv"].includes(ext)) {
    return "text";
  }
  return undefined;
}

/** Exported for the Sharing view, which shows a received image as its thumbnail. */
export function sharedThumbnailURL(id: string): string {
  return `/api/files/thumbnail?id=${encodeURIComponent(id)}`;
}

/** Re-exported so the shell does not need to import `api` just for a preview URL. */
export { thumbnailURL };
