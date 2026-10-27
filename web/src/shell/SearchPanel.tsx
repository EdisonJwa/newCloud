// Search.
//
// Scope is stated rather than implied, and it is now stated from the ANSWER rather than from the
// client's assumption (core-scope §7.2). This panel used to say "Names only" unconditionally, which
// was true when the server had no index and becomes a lie the moment it might have one. The server
// reports `mode` and `degraded` on every result, so the line says what actually ran: names, contents,
// or names with a reason why contents were not searched.
//
// Three groups, in the order people look for them:
//
//   Commands   the actions available on this screen ("Go to Trash", "New folder")
//   Folders    a folder by name, so a nested folder is reachable without walking to it
//   Files      a file by name, or by its contents when an engine is configured
//
// Recent files are the empty-query answer rather than a search result: before a query, the
// useful thing to show is what you last touched.

import { useEffect, useMemo, useRef, useState } from "react";
import { api, type Tag } from "../api";
import { useI18n } from "../i18n";
import { Icon } from "../ui";
import type { FileNode } from "../types";
import { FileTile } from "../ui";
import { shortcut } from "../platform";

export interface Command {
  id: string;
  label: string;
  /** A short right-hand hint: the shortcut, or the destination. */
  hint?: string;
  icon?: string;
  run: () => void;
}

interface Props {
  commands: Command[];
  onClose: () => void;
  onOpenFile: (entry: FileNode) => void;
  onOpenFolder: (path: string) => void;
}

type Item =
  | { kind: "command"; command: Command }
  | { kind: "folder"; node: FileNode }
  | { kind: "file"; node: FileNode };

export function SearchPanel({ commands, onClose, onOpenFile, onOpenFolder }: Props) {
  const { t, fmt } = useI18n();
  const [query, setQuery] = useState("");
  const [recent, setRecent] = useState<FileNode[]>([]);
  const [hits, setHits] = useState<FileNode[] | null>(null);
  const [searching, setSearching] = useState(false);
  const [active, setActive] = useState(0);
  // The scope line and the two filters (core-scope §4, §5, §7.1). `scope` starts as null rather
  // than as "name": before the first answer the panel has not been told which mode ran, and guessing
  // would print a claim the server has not made.
  const [scope, setScope] = useState<{ mode: string; degraded: boolean; why?: string } | null>(null);
  const [starredOnly, setStarredOnly] = useState(false);
  const [tagFilter, setTagFilter] = useState("");
  const [tags, setTags] = useState<Tag[] | null>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLUListElement>(null);
  const dialogRef = useRef<HTMLDialogElement>(null);
  const opener = useRef<Element | null>(null);

  useEffect(() => {
    opener.current = document.activeElement;
    dialogRef.current?.showModal();
    inputRef.current?.focus();
    return () => {
      if (opener.current instanceof HTMLElement) opener.current.focus();
    };
  }, []);

  useEffect(() => {
    api
      .recent(40)
      .then((res) => setRecent(res.items ?? []))
      .catch(() => setRecent([]));
  }, []);

  // The label vocabulary, fetched once when the panel opens. It is needed for the picker, and
  // fetching it here rather than on the picker's first open avoids a delay on the click that
  // matters.
  useEffect(() => {
    api
      .tags()
      .then((res) => setTags(res.items ?? []))
      .catch(() => setTags([]));
  }, []);

  // Debounced server search. The timeout keeps a fast typist from issuing one request per
  // keystroke, and the cleanup cancels a response that arrives after the query moved on.
  //
  // The query runs when a FILTER changes too, not only when the text does: a user who ticks
  // "starred only" expects the list to narrow immediately, and a filter that needs another
  // keystroke to apply reads as broken. With no text and a filter, the server treats the request as
  // a listing rather than as a search (§7.1), which is exactly what the tick means.
  useEffect(() => {
    const term = query.trim();
    const filtered = starredOnly || tagFilter !== "";
    if (term.length < 2 && !filtered) {
      setHits(null);
      setScope(null);
      setSearching(false);
      return;
    }
    let live = true;
    setSearching(true);
    const timer = window.setTimeout(() => {
      api
        .search({
          q: term,
          limit: 24,
          favorite: starredOnly || undefined,
          tag: tagFilter || undefined,
        })
        .then((res) => {
          if (!live) return;
          setHits(res.items ?? []);
          // The server names the mode that answered. `degraded_reason` is shown verbatim rather
          // than paraphrased: the server knows why it did not look, and a client-side summary of
          // that would be a second explanation that can disagree with the first.
          setScope({ mode: res.mode ?? "name", degraded: !!res.degraded, why: res.degraded_reason });
        })
        .catch(() => {
          if (live) setHits([]);
        })
        .finally(() => {
          if (live) setSearching(false);
        });
    }, 160);
    return () => {
      live = false;
      window.clearTimeout(timer);
    };
  }, [query, starredOnly, tagFilter]);

  const items = useMemo<Item[]>(() => {
    const needle = query.trim().toLowerCase();
    const commandHits = commands.filter((c) => !needle || c.label.toLowerCase().includes(needle));

    const pool = hits ?? recent;
    // With a query, the server's answer replaces the recent list. Without one, the recent list
    // is filtered locally so the list reacts before a request is worth making.
    const matching = needle && hits === null ? pool.filter((f) => f.name.toLowerCase().includes(needle)) : pool;

    const folders = matching.filter((f) => f.kind === "dir").slice(0, 8);
    const files = matching.filter((f) => f.kind === "file").slice(0, 12);

    return [
      ...commandHits.map((c) => ({ kind: "command" as const, command: c })),
      ...folders.map((node) => ({ kind: "folder" as const, node })),
      ...files.map((node) => ({ kind: "file" as const, node })),
    ];
  }, [query, commands, recent, hits]);

  useEffect(() => {
    setActive(0);
  }, [query]);

  // Keep the highlighted row in view while arrowing through a longer list.
  useEffect(() => {
    const node = listRef.current?.querySelector<HTMLElement>('[aria-selected="true"]');
    node?.scrollIntoView({ block: "nearest" });
  }, [active]);

  const run = (item: Item) => {
    if (item.kind === "command") item.command.run();
    else if (item.kind === "folder") onOpenFolder(item.node.path);
    else onOpenFile(item.node);
    onClose();
  };

  return (
    <dialog
      ref={dialogRef}
      className="serein-dialog wc-search"
      aria-labelledby="search-title"
      onCancel={(e) => {
        e.preventDefault();
        onClose();
      }}
    >
      <h2 id="search-title" className="wc-live">
        {t("search.title")}
      </h2>

      <div className="wc-search__field">
        <Icon name="search" size={15} />
        <input
          ref={inputRef}
          className="serein-input"
          value={query}
          type="text"
          role="combobox"
          aria-expanded="true"
          aria-controls="search-listbox"
          aria-autocomplete="list"
          aria-activedescendant={items[active] ? `search-item-${active}` : undefined}
          placeholder={t("search.placeholder")}
          aria-label={t("search.placeholder")}
          autoComplete="off"
          spellCheck={false}
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Escape") {
              e.preventDefault();
              onClose();
            } else if (e.key === "ArrowDown") {
              e.preventDefault();
              setActive((i) => Math.min(i + 1, Math.max(0, items.length - 1)));
            } else if (e.key === "ArrowUp") {
              e.preventDefault();
              setActive((i) => Math.max(i - 1, 0));
            } else if (e.key === "Enter") {
              e.preventDefault();
              const item = items[active];
              if (item) run(item);
            }
          }}
        />
        {query ? (
          <button
            type="button"
            className="serein-icon-btn serein-icon-btn--sm"
            aria-label={t("search.clear")}
            onClick={() => {
              setQuery("");
              inputRef.current?.focus();
            }}
          >
            <Icon name="close" size={12} />
          </button>
        ) : (
          <kbd className="serein-kbd">{shortcut("K")}</kbd>
        )}
      </div>

      {/* The filter row (core-scope §4, §5, §7.1). Two controls, both of which the SERVER applies:
          a client-side filter over the 24 rows already on screen would hide the fact that the
          twenty-fifth match exists. */}
      <div className="wc-search__filters">
        <button
          type="button"
          className={`wc-filterchip${starredOnly ? " wc-filterchip--on" : ""}`}
          aria-pressed={starredOnly}
          onClick={() => setStarredOnly((v) => !v)}
        >
          <Icon name={starredOnly ? "starFill" : "star"} size={11} />
          {t("social.filterStarred")}
        </button>
        {tags && tags.length > 0 ? (
          <label className="wc-search__tagpick">
            <Icon name="tag" size={11} />
            <span className="serein-sr-only">{t("social.filterTag")}</span>
            <select
              className="serein-select serein-select--sm"
              value={tagFilter}
              onChange={(e) => setTagFilter(e.target.value)}
            >
              <option value="">{t("social.allTags")}</option>
              {tags.map((tag) => (
                <option key={tag.id} value={tag.id}>
                  {tag.name}
                </option>
              ))}
            </select>
          </label>
        ) : null}
      </div>

      <div className="wc-search__scope">
        {query.trim().length >= 2 || starredOnly || tagFilter ? (
          <span>
            {scope === null
              ? t("search.scopePending")
              : scope.mode === "content" || scope.mode === "all"
                ? t("search.namesAndContents")
                : scope.degraded
                  ? t("search.namesOnlyDegraded", { why: scope.why ?? "" })
                  : t("search.namesOnly")}
          </span>
        ) : (
          <span>{t("search.recentHint")}</span>
        )}
        {searching ? <span className="serein-spinner" aria-hidden="true" /> : null}
      </div>

      {items.length === 0 ? (
        <p className="wc-search__empty">
          {query.trim().length >= 2 ? t("search.none", { q: query.trim() }) : t("search.noRecent")}
        </p>
      ) : (
        <ul ref={listRef} className="wc-search__list" id="search-listbox" role="listbox" aria-label={t("search.title")}>
          {items.map((item, i) => {
            const previous = items[i - 1];
            const heading =
              item.kind !== previous?.kind
                ? item.kind === "command"
                  ? t("search.commands")
                  : item.kind === "folder"
                    ? t("search.folders")
                    : t("search.files")
                : null;
            const label =
              item.kind === "command"
                ? item.command.label
                : item.node.name;
            return (
              <li key={`${item.kind}-${item.kind === "command" ? item.command.id : item.node.id}`}>
                {heading ? (
                  <div className="wc-search__group" role="presentation">
                    {heading}
                  </div>
                ) : null}
                <div
                  id={`search-item-${i}`}
                  role="option"
                  aria-selected={i === active}
                  className="wc-search__item"
                  onMouseEnter={() => setActive(i)}
                  onClick={() => run(item)}
                >
                  {item.kind === "command" ? (
                    <span className="wc-search__cmdicon" aria-hidden="true">
                      <Icon name={item.command.icon ?? "command"} size={14} />
                    </span>
                  ) : (
                    <FileTile entry={item.node} size={28} />
                  )}
                  <span className="wc-search__main">
                    <span className="wc-search__label">{label}</span>
                    <span className="wc-search__sub">
                      {item.kind === "command"
                        ? (item.command.hint ?? "")
                        : item.kind === "folder"
                          ? t("search.folderSub", { path: parentOf(item.node.path) || t("files.root") })
                          : `${parentOf(item.node.path) || t("files.root")} · ${fmt.size(item.node.size)}`}
                    </span>
                  </span>
                  {item.kind === "command" && item.command.hint ? (
                    <span className="wc-search__hint">{item.command.hint}</span>
                  ) : null}
                </div>
              </li>
            );
          })}
        </ul>
      )}

      <div className="wc-search__foot">
        <span>{t("search.footNav")}</span>
        <span>{t("search.footOpen")}</span>
        <span>{t("search.footClose")}</span>
      </div>
    </dialog>
  );
}

function parentOf(path: string): string {
  const clean = path.replace(/^\//, "");
  const i = clean.lastIndexOf("/");
  return i < 0 ? "" : clean.slice(0, i);
}
