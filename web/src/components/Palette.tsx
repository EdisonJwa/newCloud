// Command palette (⌘K).
//
// Scope is stated rather than implied: it searches commands and recent files. There is no
// server-side full-text index, so pretending to search "everything" would return a
// confident-looking subset — the worst of both. Recent files cover the "I know the name" case
// for the files people actually go back to.

import { useEffect, useMemo, useRef, useState } from "react";
import { api } from "../api";
import { useI18n } from "../i18n";
import { Icon } from "../ui";
import type { FileNode } from "../types";

export interface Command {
  id: string;
  label: string;
  hint?: string;
  run: () => void;
}

interface Props {
  commands: Command[];
  onClose: () => void;
  onOpenPath: (path: string) => void;
}

export function Palette({ commands, onClose, onOpenPath }: Props) {
  const { t } = useI18n();
  const [query, setQuery] = useState("");
  const [files, setFiles] = useState<FileNode[]>([]);
  const [active, setActive] = useState(0);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLUListElement>(null);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        onClose();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  useEffect(() => {
    inputRef.current?.focus();
    api
      .recent(60)
      .then((res) => setFiles(res.items ?? []))
      .catch(() => setFiles([]));
  }, []);

  const items = useMemo(() => {
    const needle = query.trim().toLowerCase();
    const commandHits = commands.filter((c) => !needle || c.label.toLowerCase().includes(needle));
    const fileHits = files
      .filter((f) => !needle || f.name.toLowerCase().includes(needle) || f.path.toLowerCase().includes(needle))
      .slice(0, 8);
    return [
      ...commandHits.map((c) => ({ kind: t("palette.commands"), label: c.label, hint: c.hint, run: c.run })),
      ...fileHits.map((f) => ({
        kind: t("palette.files"),
        label: f.name,
        hint: f.path,
        run: () => onOpenPath(f.path),
      })),
    ];
  }, [query, commands, files, t, onOpenPath]);

  useEffect(() => {
    setActive(0);
  }, [query]);

  // Keep the highlighted row in view while arrowing through a longer list.
  useEffect(() => {
    const node = listRef.current?.querySelector<HTMLElement>('[aria-selected="true"]');
    node?.scrollIntoView({ block: "nearest" });
  }, [active]);

  return (
    <>
      <div className="scrim" onClick={onClose} />
      <div className="palette" role="dialog" aria-modal="true" aria-label={t("palette.placeholder")}>
        <input
          ref={inputRef}
          value={query}
          placeholder={t("palette.placeholder")}
          aria-label={t("palette.placeholder")}
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Escape") {
              e.preventDefault();
              onClose();
            } else if (e.key === "ArrowDown") {
              e.preventDefault();
              setActive((i) => Math.min(i + 1, items.length - 1));
            } else if (e.key === "ArrowUp") {
              e.preventDefault();
              setActive((i) => Math.max(i - 1, 0));
            } else if (e.key === "Enter") {
              e.preventDefault();
              const item = items[active];
              if (item) {
                item.run();
                onClose();
              }
            }
          }}
        />

        {items.length === 0 ? (
          <div className="none">{t("palette.empty")}</div>
        ) : (
          <ul ref={listRef} role="listbox" aria-label={t("palette.placeholder")}>
            {items.map((item, i) => (
              <li
                key={`${item.kind}-${item.label}-${i}`}
                role="option"
                aria-selected={i === active}
                onMouseEnter={() => setActive(i)}
                onClick={() => {
                  item.run();
                  onClose();
                }}
              >
                <span className="kind">{item.kind}</span>
                <span className="label">{item.label}</span>
                {item.hint ? <span className="hint">{item.hint}</span> : null}
              </li>
            ))}
          </ul>
        )}

        <div className="foot">
          <span>{t("palette.nav")}</span>
          <span>{t("palette.run")}</span>
          <span>{t("palette.close")}</span>
          <span className="spacer" />
          <Icon name="command" size={11} />
        </div>
      </div>
    </>
  );
}
