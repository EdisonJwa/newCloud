// Immersive preview.
//
// Not a modal with a title bar: the content takes the whole screen, the metadata is reduced to
// one thin strip, and the siblings in the same folder sit along the bottom as a filmstrip.
// This is the only way "look at a photo" works inside a file manager — a bordered dialog
// forces the picture to fight the chrome for space.

import { useEffect, useRef, useState } from "react";
import { downloadURL, inlineURL, thumbnailURL } from "../api";
import { useI18n } from "../i18n";
import { Icon } from "../ui";
import type { FileNode } from "../types";

interface Props {
  entries: FileNode[];
  index: number;
  onIndex: (index: number) => void;
  onClose: () => void;
}

export function Viewer({ entries, index, onIndex, onClose }: Props) {
  const { t, fmt } = useI18n();
  const entry = entries[index];
  const closeRef = useRef<HTMLButtonElement>(null);

  // The dialog is a fixed overlay rather than <dialog> because it needs a three-row grid
  // (bar, stage, filmstrip) that the element's default centring fights with. Focus is moved
  // in explicitly, and Escape is handled here.
  useEffect(() => {
    closeRef.current?.focus();
  }, []);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        onClose();
      } else if (e.key === "ArrowLeft") {
        e.preventDefault();
        if (index > 0) onIndex(index - 1);
      } else if (e.key === "ArrowRight") {
        e.preventDefault();
        if (index < entries.length - 1) onIndex(index + 1);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [index, entries.length, onIndex, onClose]);

  if (!entry) return null;

  return (
    <div className="viewer" role="dialog" aria-modal="true" aria-label={entry.name}>
      <div className="bar">
        <span className="name">{entry.name}</span>
        <span className="sub">
          {fmt.size(entry.size)} · {fmt.relative(entry.modified_at)}
        </span>
        <span className="spacer" />
        <span className="sub mono">
          {index + 1}/{entries.length}
        </span>
        <a className="btn sm" href={downloadURL(entry.path)} download>
          <Icon name="download" size={12} />
          {t("preview.download")}
        </a>
        <button ref={closeRef} type="button" onClick={onClose} aria-label={t("preview.close")}>
          <Icon name="close" size={13} />
          {t("preview.close")}
        </button>
      </div>

      <div className="stage">
        <Stage entry={entry} />
      </div>

      {entries.length > 1 ? (
        <div className="filmstrip" role="tablist" aria-label={t("preview.prev")}>
          {entries.map((item, i) => (
            <button
              key={item.id}
              type="button"
              role="tab"
              aria-current={i === index}
              aria-label={item.name}
              onClick={() => onIndex(i)}
            >
              {item.preview === "image" ? (
                <img src={thumbnailURL(item.path)} alt="" loading="lazy" />
              ) : (
                <span>{(item.name.split(".").pop() ?? "").slice(0, 3)}</span>
              )}
            </button>
          ))}
        </div>
      ) : null}
    </div>
  );
}

function Stage({ entry }: { entry: FileNode }) {
  const { t } = useI18n();
  const [text, setText] = useState<string | null>(null);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    setText(null);
    setFailed(false);
    if (entry.preview !== "text") return;

    let live = true;
    fetch(inlineURL(entry.path), { credentials: "same-origin" })
      .then((r) => (r.ok ? r.text() : Promise.reject(new Error(String(r.status)))))
      .then((body) => {
        // Only a prefix is shown: a 200 MB log would lock the tab, and the point of the
        // preview is to recognise the file, not to read it in full.
        if (live) setText(body.slice(0, 100_000));
      })
      .catch(() => {
        if (live) setFailed(true);
      });
    return () => {
      live = false;
    };
  }, [entry.path, entry.preview]);

  if (failed) return <Note text={t("preview.unavailable")} />;

  switch (entry.preview) {
    case "image":
      return (
        <img
          src={inlineURL(entry.path)}
          alt={entry.name}
          onError={() => setFailed(true)}
        />
      );
    case "video":
      return <video src={inlineURL(entry.path)} controls autoPlay={false} onError={() => setFailed(true)} />;
    case "audio":
      return <audio src={inlineURL(entry.path)} controls onError={() => setFailed(true)} />;
    case "pdf":
      return <iframe src={inlineURL(entry.path)} title={entry.name} />;
    case "text":
      return text === null ? <Note text={t("state.loading")} /> : <pre>{text}</pre>;
    default:
      return <Note text={t("preview.none")} />;
  }
}

function Note({ text }: { text: string }) {
  return <div className="viewer-note">{text}</div>;
}
