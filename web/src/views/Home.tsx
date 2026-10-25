// Home.
//
// The question Home answers is "what was I doing?". Everything on it is a route into work in
// progress, not a dashboard of decoration: every number opens the thing it counts, and every
// section has a destination for the full list.
//
// ## The resume band, and why its label changes
//
// The band leads with the file you last opened, when there is one. When there is not, it leads
// with the most recently modified file and says **Recently modified** instead of claiming a
// history it does not have. That distinction is the whole point: a "Continue where you left
// off" heading over a file this session has never opened is a small lie that makes the honest
// parts of the interface worth less.
//
// The record is per browser (localStorage), not per account. It is therefore described as
// "last opened on this device" on the settings surface and in the docs rather than as a
// synchronised history, which it is not.
//
// ## Compact by measurement
//
// The band is 94px, not a 371px media panel: on a 390px phone the previous version put the
// whole first viewport into one file, so the first real row was below the fold. Below the band
// a 65/35 split gives Recent a compact row list and Shared with me a narrower received column,
// with Activity under both.

import { useCallback, useEffect, useMemo, useState } from "react";
import { api, APIError, downloadURL } from "../api";
import { useI18n } from "../i18n";
import {
  EmptyState,
  ErrorState,
  FileTile,
  Icon,
  Skeleton,
  kindLabel,
} from "../ui";
import type { AuditEvent, FileNode, Me, Share, SharedNode } from "../types";
import type { Route } from "../route";
import { accessLabel, auditTarget, parentOf, previewOf } from "../nodes";

interface Props {
  me: Me | null;
  reloadKey: number;
  onRoute: (changes: Partial<Route>, mode: "push" | "replace") => void;
  onOpenFile: (entry: FileNode, from: Route["from"]) => void;
  onOpenReceived: (node: SharedNode) => void;
  onUpload: () => void;
  pendingAction: "upload" | "mkdir" | null;
  onActionHandled: () => void;
}

/* -------------------------------------------------------- last opened record --- */

const OPENED_KEY = "wc.lastOpened";

export interface OpenedRecord {
  id: string;
  name: string;
  path: string;
  /** Enough to draw the tile without a request. */
  preview?: FileNode["preview"];
  kind: "file" | "dir";
  size: number;
  at: number;
}

/**
 * recordOpened remembers the last file this browser opened.
 *
 * `at` is the *opening*, not the file's modification time: the band says "you opened this",
 * and using the file's mtime would make it claim the person touched something they never did.
 */
export function recordOpened(entry: FileNode): void {
  if (entry.kind !== "file") return;
  try {
    const record: OpenedRecord = {
      id: entry.id,
      name: entry.name,
      path: entry.path,
      preview: entry.preview,
      kind: "file",
      size: entry.size,
      at: Date.now(),
    };
    localStorage.setItem(OPENED_KEY, JSON.stringify(record));
  } catch {
    /* private mode: no resume history, which the band then correctly does not claim */
  }
}

function readOpened(): OpenedRecord | null {
  try {
    const raw = localStorage.getItem(OPENED_KEY);
    if (!raw) return null;
    const parsed: unknown = JSON.parse(raw);
    if (typeof parsed !== "object" || parsed === null) return null;
    const r = parsed as Partial<OpenedRecord>;
    if (
      typeof r.id !== "string" ||
      typeof r.name !== "string" ||
      typeof r.path !== "string" ||
      typeof r.size !== "number" ||
      typeof r.at !== "number"
    ) {
      return null;
    }
    return r as OpenedRecord;
  } catch {
    return null;
  }
}

/* ------------------------------------------------------------------- view --- */

export function Home({
  me,
  reloadKey,
  onRoute,
  onOpenFile,
  onOpenReceived,
  onUpload,
  pendingAction,
  onActionHandled,
}: Props) {
  const { t, fmt } = useI18n();
  const [recent, setRecent] = useState<FileNode[] | null>(null);
  const [received, setReceived] = useState<SharedNode[] | null>(null);
  const [links, setLinks] = useState<Share[] | null>(null);
  const [events, setEvents] = useState<AuditEvent[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [opened, setOpened] = useState<OpenedRecord | null>(() => readOpened());

  const load = useCallback(() => {
    setError(null);
    Promise.all([api.recent(8), api.sharedWithMe(), api.shares(), api.audit({ offset: 0 })])
      .then(([recents, shares, shareList, audit]) => {
        setRecent(recents.items ?? []);
        setReceived(shares.items ?? []);
        setLinks((shareList.items ?? []).filter((s) => !s.revoked_at));
        setEvents(audit.events ?? []);
      })
      .catch((err) => {
        setError(err instanceof APIError ? err.message || err.code : String(err));
        setRecent([]);
        setReceived([]);
        setLinks([]);
        setEvents([]);
      });
  }, []);

  useEffect(load, [load, reloadKey]);

  useEffect(() => {
    setOpened(readOpened());
  }, [reloadKey]);

  useEffect(() => {
    if (pendingAction === "upload") {
      onUpload();
      onActionHandled();
    }
  }, [pendingAction, onUpload, onActionHandled]);

  // The band's subject: the last opened file if it is still in the recent set (so the row has
  // a real path and a preview), otherwise that record itself, otherwise the newest file.
  //
  // `openedAt` travels with the subject in every case. The eyebrow and the facts line have to
  // agree: a band that says "Continue where you left off" and then reports a *modified* time
  // contradicts itself in the same 94px, and the earlier version did exactly that whenever the
  // opened file was also in Recent.
  const resume = useMemo<{
    entry: FileNode | OpenedRecord;
    resumed: boolean;
    openedAt: number | null;
  } | null>(() => {
    if (!recent) return null;
    if (opened) {
      const match = recent.find((f) => f.path === opened.path && f.id === opened.id);
      if (match) return { entry: match, resumed: true, openedAt: opened.at };
      // Still on this device but no longer in Recent: the record itself is accurate, and the
      // band says "last opened on this device" rather than implying live state.
      return { entry: opened, resumed: true, openedAt: opened.at };
    }
    const newest = recent.find((f) => f.kind === "file");
    return newest ? { entry: newest, resumed: false, openedAt: null } : null;
  }, [recent, opened]);

  const used = me?.used_bytes ?? 0;
  const quota = me?.quota_bytes ?? 0;
  const percent = quota > 0 ? Math.min(100, Math.round((used / quota) * 100)) : 0;
  const activeLinks = (links ?? []).filter((s) => !s.revoked_at);

  if (error) {
    return (
      <div className="serein-page serein-page--home">
        <ErrorState message={error} onRetry={load} />
      </div>
    );
  }

  const greeting = greetingKey(new Date());

  return (
    <div className="serein-page serein-page--home">
      <div className="serein-page-header">
        <div className="serein-page-header__main">
          <p className="serein-page-header__eyebrow">{t(greeting)}</p>
          <h1 className="serein-page-header__title">{displayNameOf(me)}</h1>
          <p className="serein-page-header__description">
            {quota > 0
              ? t("home.headerQuota", { used: fmt.size(used), quota: fmt.size(quota) })
              : t("home.headerUsed", { used: fmt.size(used) })}
          </p>
        </div>
        <div className="serein-page-header__actions">
          <button type="button" className="serein-btn serein-btn--outline" onClick={() => onRoute({ screen: "files" }, "push")}>
            <Icon name="files" size={13} />
            <span className="serein-btn__label">{t("home.browse")}</span>
          </button>
          <button type="button" className="serein-btn" onClick={onUpload}>
            <Icon name="upload" size={13} />
            <span className="serein-btn__label">{t("files.upload")}</span>
          </button>
        </div>
      </div>

      {/* The resume band. One row, 94px, regardless of format. */}
      {recent === null ? (
        <div className="wc-resume-skeleton">
          <span className="serein-skeleton" style={{ width: 112, height: 74, borderRadius: 8 }} />
          <span className="wc-skeleton-row__stack">
            <span className="serein-skeleton serein-skeleton--title" style={{ width: "42%" }} />
            <span className="serein-skeleton serein-skeleton--text" style={{ width: "26%" }} />
            <span className="serein-skeleton serein-skeleton--text" style={{ width: "58%" }} />
          </span>
        </div>
      ) : resume ? (
        <ResumeBand
          entry={resume.entry}
          resumed={resume.resumed}
          openedAt={resume.openedAt}
          onOpen={() => {
            if (resume.entry.kind === "file" && "path" in resume.entry) {
              // The collection is Recent, so Next walks the workspace in recent order — which
              // is exactly the set the band was drawn from.
              onOpenFile(resume.entry as FileNode, { kind: "recent" });
            }
          }}
          onDownload={() => {
            window.location.href = downloadURL(resume.entry.path);
          }}
        />
      ) : (
        <div className="serein-empty" style={{ padding: "34px 20px" }}>
          <span className="serein-empty__icon">
            <Icon name="upload" size={22} />
          </span>
          <p className="serein-empty__title">{t("home.nothingYetTitle")}</p>
          <p className="serein-empty__description">{t("home.nothingYetHint")}</p>
          <div className="serein-empty__actions">
            <button type="button" className="serein-btn" onClick={onUpload}>
              {t("files.upload")}
            </button>
          </div>
        </div>
      )}

      <div className="serein-home-grid wc-home-grid">
        {/* Recent */}
        <section className="serein-section" aria-labelledby="home-recent">
          <div className="serein-section__head">
            <h2 className="wc-section__title" id="home-recent">
              {t("home.recent")}
            </h2>
            <span className="serein-section__meta">
              {recent ? t("home.countFiles", { n: recent.length }) : ""}
            </span>
            <button
              type="button"
              className="wc-section__link"
              onClick={() => onRoute({ screen: "files", space: "", kind: "all" }, "push")}
            >
              {t("home.allFiles")} →
            </button>
          </div>

          {recent === null ? (
            <Skeleton rows={5} />
          ) : recent.length === 0 ? (
            <EmptyState
              icon="upload"
              title={t("home.recentEmpty")}
              hint={t("home.recentEmptyHint")}
              actions={
                <button type="button" className="serein-btn serein-btn--sm" onClick={onUpload}>
                  {t("files.upload")}
                </button>
              }
            />
          ) : (
            <div className="wc-card">
              <div className="wc-list">
                {recent.map((file) => (
                  <button
                    key={file.id + file.path}
                    type="button"
                    className="wc-list__row"
                    onClick={() => onOpenFile(file, { kind: "recent" })}
                    title={file.path}
                  >
                    <FileTile entry={file} size={34} />
                    <span className="wc-list__main">
                      <span className="wc-list__title">{file.name}</span>
                      <span className="wc-list__meta">
                        {kindLabel(t, file)} · {parentOf(file.path) || t("files.root")}
                      </span>
                    </span>
                    <span className="wc-list__end">
                      <span className="wc-list__end--secondary">{fmt.size(file.size)}</span>
                      <span>{fmt.relative(file.modified_at)}</span>
                    </span>
                  </button>
                ))}
              </div>
            </div>
          )}
        </section>

        {/* Shared with me — the received direction, never the outbound one. */}
        <section className="serein-section" aria-labelledby="home-received">
          <div className="serein-section__head">
            <h2 className="wc-section__title" id="home-received">
              {t("home.receivedTitle")}
            </h2>
            <button
              type="button"
              className="wc-section__link"
              // The received side explicitly: this link previously opened the outbound link
              // manager, so it promised received files and delivered public links.
              onClick={() => onRoute({ screen: "sharing", tab: "received" }, "push")}
            >
              {t("home.seeAll")} →
            </button>
          </div>

          {received === null ? (
            <Skeleton rows={4} />
          ) : received.length === 0 ? (
            <EmptyState
              icon="shares"
              title={t("received.empty")}
              hint={t("received.emptyHint")}
            />
          ) : (
            <div className="wc-card">
              <div className="wc-list">
                {received.slice(0, 5).map((node) => (
                  <button
                    key={node.id}
                    type="button"
                    className="wc-list__row"
                    onClick={() => onOpenReceived(node)}
                  >
                    <FileTile
                      entry={{
                        kind: node.kind === "dir" ? "dir" : "file",
                        preview: previewOf(node.mime, node.name),
                        name: node.name,
                        path: "",
                      }}
                      size={34}
                    />
                    <span className="wc-list__main">
                      <span className="wc-list__title">{node.name}</span>
                      <span className="wc-list__meta">{accessLabel(t, node.permissions)}</span>
                    </span>
                    <span className="wc-list__end">
                      {node.modified_at ? fmt.relative(node.modified_at) : ""}
                    </span>
                  </button>
                ))}
              </div>
            </div>
          )}
        </section>
      </div>

      {/* Links I manage — compact, because Home is about the person's own work. */}
      <section className="serein-section" aria-labelledby="home-links">
        <div className="serein-section__head">
          <h2 className="wc-section__title" id="home-links">
            {t("home.linksTitle")}
          </h2>
          <span className="serein-section__meta">
            {links ? t("home.countActive", { n: activeLinks.length }) : ""}
          </span>
          <button
            type="button"
            className="wc-section__link"
            onClick={() => onRoute({ screen: "sharing", tab: "managed" }, "push")}
          >
            {t("home.manageLinks")} →
          </button>
        </div>

        {links === null ? (
          <Skeleton rows={2} />
        ) : activeLinks.length === 0 ? (
          <EmptyState icon="link" title={t("shared.empty")} hint={t("shared.emptyHint")} />
        ) : (
          <div className="wc-card">
            <div className="wc-list">
              {activeLinks.slice(0, 4).map((share) => {
                const max = share.max_downloads ?? share.max_uploads;
                const count = share.download_count + share.upload_count;
                return (
                  <button
                    key={share.id}
                    type="button"
                    className="wc-list__row"
                    onClick={() => onRoute({ screen: "sharing", tab: "managed", share: share.id, inspector: true, pane: "share" }, "push")}
                  >
                    <span className="serein-inspector__callout-icon" aria-hidden="true">
                      <Icon name={share.kind === "request" ? "inbox" : "link"} size={14} />
                    </span>
                    <span className="wc-list__main">
                      <span className="wc-list__title">{share.label || share.node_path}</span>
                      <span className="wc-list__meta">
                        {share.kind === "request"
                          ? t("home.request")
                          : share.kind === "email"
                            ? t("home.email")
                            : t("home.link")}
                        {share.has_password ? ` · ${t("inspector.protected")}` : ""}
                        {share.expires_at ? ` · ${t("home.expires", { date: fmt.date(share.expires_at) })}` : ""}
                      </span>
                    </span>
                    <span className="wc-list__end">
                      {max ? t("home.uses", { used: count, max }) : t("home.unlimited")}
                    </span>
                  </button>
                );
              })}
            </div>
          </div>
        )}
      </section>

      {/* Activity — the product's distinguishing claim made visible on the landing surface. */}
      <section className="serein-section" aria-labelledby="home-activity">
        <div className="serein-section__head">
          <h2 className="wc-section__title" id="home-activity">
            {t("home.activityTitle")}
          </h2>
          <button
            type="button"
            className="wc-section__link"
            onClick={() => onRoute({ screen: "settings", settingsTab: "activity" }, "push")}
          >
            {t("home.seeAll")} →
          </button>
        </div>

        {events === null ? (
          <Skeleton rows={3} />
        ) : events.length === 0 ? (
          <EmptyState icon="pulse" title={t("activity.empty")} />
        ) : (
          <div className="wc-card">
            <div className="wc-list">
              {events.slice(0, 5).map((event) => (
                <div className="wc-list__row" key={event.id}>
                  <span
                    className={`serein-status-dot${event.result === "denied" ? " serein-status-dot--danger" : ""}`}
                    aria-hidden="true"
                  />
                  <span className="wc-list__main">
                    <span className="wc-list__title">{event.action}</span>
                    <span className="wc-list__meta">{auditTarget(event, t)}</span>
                  </span>
                  <span className="wc-list__end">{fmt.relative(event.ts)}</span>
                </div>
              ))}
            </div>
          </div>
        )}
      </section>

      {/* Storage. The only number here that is not a route; it is a fact about the account. */}
      <section className="serein-section" aria-labelledby="home-storage">
        <div className="serein-section__head">
          <h2 className="wc-section__title" id="home-storage">
            {t("home.storage")}
          </h2>
          <span className="serein-section__meta">
            {quota > 0 ? t("home.storageOf", { used: fmt.size(used), quota: fmt.size(quota) }) : fmt.size(used)}
          </span>
        </div>
        <div className="wc-card wc-card__padding">
          {quota > 0 ? (
            <>
              <div
                className={`serein-meter${percent >= 90 ? " serein-meter--danger" : percent >= 75 ? " serein-meter--warning" : ""}`}
                role="progressbar"
                aria-valuemin={0}
                aria-valuemax={100}
                aria-valuenow={percent}
                aria-label={t("home.storageUsed", { percent })}
              >
                <span className="serein-meter__bar" style={{ width: `${percent}%` }} />
              </div>
              <p className="serein-hint wc-storage-note">
                {t("home.storagePercent", { percent })}
                {percent >= 90 ? ` · ${t("home.storageFull")}` : ""}
              </p>
            </>
          ) : (
            <p className="serein-hint">{t("home.unlimited")}</p>
          )}
        </div>
      </section>

      {pendingAction === "mkdir" ? <NoopOnHome onDone={onActionHandled} /> : null}
    </div>
  );
}

/**
 * NoopOnHome consumes a "new folder" request that arrived while Home was the active surface.
 *
 * New folder is only meaningful inside a folder, so Home routes there instead of pretending to
 * create one at the root. Rendering nothing keeps the request from being silently dropped and
 * retried on the next navigation.
 */
function NoopOnHome({ onDone }: { onDone: () => void }) {
  useEffect(() => {
    onDone();
  }, [onDone]);
  return null;
}

/* ------------------------------------------------------------- resume band --- */

function ResumeBand({
  entry,
  resumed,
  openedAt,
  onOpen,
  onDownload,
}: {
  entry: FileNode | OpenedRecord;
  resumed: boolean;
  /** When this browser last opened the file. `null` means the band is showing a modification. */
  openedAt: number | null;
  onOpen: () => void;
  onDownload: () => void;
}) {
  const { t, fmt } = useI18n();
  const file = entry as FileNode;
  const isDir = entry.kind === "dir";

  return (
    <section className="serein-resume wc-resume" aria-labelledby="home-resume">
      <div className="serein-resume__preview">
        <FileTile entry={{ ...file, kind: "file" }} size={112} />
      </div>

      <div className="serein-resume__main">
        <p className="serein-resume__eyebrow" id="home-resume">
          {/* The distinction the whole band exists for. */}
          {resumed ? t("home.resumeEyebrow") : t("home.recentlyModified")}
        </p>
        <p className="serein-resume__title">{file.name}</p>
        <p className="serein-resume__where">
          <Icon name="files" size={11} />
          {parentOf(file.path) || t("files.root")}
        </p>
        <p className="serein-resume__facts">
          {openedAt !== null
            ? t("home.openedAt", {
                when: fmt.relative(new Date(openedAt).toISOString()),
                kind: kindLabel(t, file),
                size: fmt.size(file.size),
              })
            : t("home.modifiedAt", {
                when: fmt.relative(file.modified_at),
                kind: kindLabel(t, file),
                size: fmt.size(file.size),
              })}
        </p>
      </div>

      <div className="serein-resume__actions">
        {!isDir ? (
          <button
            type="button"
            className="serein-icon-btn"
            aria-label={t("detail.download")}
            title={t("detail.download")}
            onClick={onDownload}
          >
            <Icon name="download" size={15} />
          </button>
        ) : null}
        <button type="button" className="serein-btn" onClick={onOpen}>
          {isDir ? t("files.openFolder") : t("home.openFile")}
        </button>
      </div>
    </section>
  );
}

/* ---------------------------------------------------------------- helpers --- */

function greetingKey(now: Date): string {
  const h = now.getHours();
  if (h < 5) return "home.greetingNight";
  if (h < 12) return "home.greetingMorning";
  if (h < 18) return "home.greetingAfternoon";
  return "home.greetingEvening";
}

export function displayNameOf(me: Me | null): string {
  return me?.display_name || me?.username || me?.email || "";
}
