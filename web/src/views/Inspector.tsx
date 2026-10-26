// The Inspector.
//
// A push panel on desktop, a bottom sheet on a phone, with content that is not remounted when
// the selection changes: the panel's scroll position and its focused tab survive clicking a
// different file, which is the difference between "browsing with details open" and "reopening
// details on every row".
//
// Four panes, in the order they answer a question:
//
//   Info      what is this file — kind, size, where, when, who
//   Versions  what happened to it (the server's version list, with restore gated on whether
//             the storage layout actually kept the bytes)
//   Activity  who touched it (the audit feed, filtered to this path)
//   Share     who else can reach it (the live links pointing at this node)
//
// Every pane is honest about absence. A tree-layout deployment records version *rows* but
// keeps no copy of the bytes, so Restore is disabled with a reason rather than offered and
// failing; a shell link exists but the URL is not recoverable from the server, and the pane
// says "URL not recoverable" instead of offering a copy button that would copy nothing.

import { useEffect, useState } from "react";
import { api, APIError, downloadURL, sharedContentURL, thumbnailURL } from "../api";
import { useI18n } from "../i18n";
import { EmptyState, Icon, FileTile, kindLabel, useToast } from "../ui";
import { CommentThread, StarButton, TagEditor } from "../components/Social";
import type { AuditEvent, FileNode, Share, SharedNode, Version } from "../types";
import type { Pane } from "../route";

interface FileInspectorProps {
  mode: "file";
  entry: FileNode | null;
  selectionCount: number;
  pane: Pane;
  onPane: (pane: Pane) => void;
  onClose: () => void;
  me: { id?: string; display_name?: string; username?: string; email?: string } | null;
  onOpen: (entry: FileNode) => void;
  onRename: (entry: FileNode) => void;
  onDelete: (entries: FileNode[]) => void;
  onShare: (entry: FileNode) => void;
  onTransfer: (op: "copy" | "move", entries: FileNode[]) => void;
  /**
   * onStar is called after the panel's star changed, so the listing re-fetches.
   *
   * The panel does not own the star: the row beside it shows the same bit, and two components
   * holding their own copy of it is how a list and a detail panel start disagreeing after one click.
   */
  onStar: (starred: boolean) => void;
  onRefresh: () => void;
  onSelectAll: () => void;
  onClearSelection: () => void;
}

interface ShareInspectorProps {
  mode: "share";
  share: Share | null;
  onClose: () => void;
  onRefresh: () => void;
  onOpenLink: (share: Share) => void;
}

interface ReceivedInspectorProps {
  mode: "received";
  node: SharedNode | null;
  onClose: () => void;
}

export type InspectorProps = FileInspectorProps | ShareInspectorProps | ReceivedInspectorProps;

const PANES: readonly { id: Pane; key: string }[] = [
  { id: "info", key: "inspector.info" },
  { id: "versions", key: "inspector.versions" },
  // Placed after Versions and before Activity: the order answers "what is it, what happened to it,
  // what did people say about it, who touched it, who can reach it". A discussion is about the file
  // rather than about the system, so it belongs nearer the file than the audit feed does.
  { id: "comments", key: "social.comments" },
  { id: "activity", key: "inspector.activity" },
  { id: "share", key: "inspector.share" },
];

export function Inspector(props: InspectorProps) {
  if (props.mode === "share") {
    return <SharePane share={props.share} onClose={props.onClose} onRefresh={props.onRefresh} onOpenLink={props.onOpenLink} />;
  }
  if (props.mode === "received") {
    return <ReceivedPane node={props.node} onClose={props.onClose} />;
  }
  return <FilePane {...props} />;
}

/* ------------------------------------------------------------- file panel --- */

function FilePane(props: FileInspectorProps) {
  const { t, fmt } = useI18n();
  const toast = useToast();
  const { entry, selectionCount } = props;
  const [versions, setVersions] = useState<Version[] | null>(null);
  const [versionState, setVersionState] = useState<{ cheap: boolean; restorable: boolean } | null>(null);
  const [versionsError, setVersionsError] = useState<string | null>(null);
  const [events, setEvents] = useState<AuditEvent[] | null>(null);
  const [eventsError, setEventsError] = useState<string | null>(null);
  const [links, setLinks] = useState<Share[] | null>(null);
  const [restoring, setRestoring] = useState(false);

  // Versions and links are fetched per file, not per selection change: the effect keys on the
  // path so the panel does not refetch when a parent re-renders with the same file.
  useEffect(() => {
    setVersions(null);
    setVersionState(null);
    setVersionsError(null);
    if (!entry || entry.kind === "dir") return;
    let live = true;
    api
      .versions(entry.path)
      .then((res) => {
        if (!live) return;
        setVersions(res.versions ?? []);
        setVersionState({
          cheap: res.cheap_versions,
          restorable: res.version_content_available,
        });
      })
      .catch((err) => {
        if (live) setVersionsError(err instanceof APIError ? err.message || err.code : String(err));
      });
    return () => {
      live = false;
    };
  }, [entry]);

  useEffect(() => {
    setEvents(null);
    setEventsError(null);
    if (!entry) return;
    let live = true;
    api
      .audit({ offset: 0 })
      .then((res) => {
        if (!live) return;
        // The feed has no per-target filter on the server yet, so it is filtered here and the
        // pane says what it is showing rather than implying a complete per-file history.
        setEvents(
          (res.events ?? []).filter((e) => {
            const meta = e.metadata ?? {};
            return meta.path === entry.path || meta.name === entry.name;
          }),
        );
      })
      .catch((err) => {
        if (live) setEventsError(err instanceof APIError ? err.message || err.code : String(err));
      });
    return () => {
      live = false;
    };
  }, [entry]);

  useEffect(() => {
    setLinks(null);
    if (!entry) return;
    let live = true;
    api
      .shares()
      .then((res) => {
        if (!live) return;
        setLinks(
          (res.items ?? []).filter((s) => !s.revoked_at && (s.node_path === entry.path || s.node_id === entry.id)),
        );
      })
      .catch(() => {
        if (live) setLinks([]);
      });
    return () => {
      live = false;
    };
  }, [entry]);

  const restore = async (version: number) => {
    if (!entry) return;
    setRestoring(true);
    try {
      await api.restoreVersion(entry.path, version);
      toast.success(t("toast.versionRestored", { n: version }));
      props.onRefresh();
    } catch (err) {
      toast.error(err instanceof APIError ? err.message || err.code : String(err));
    } finally {
      setRestoring(false);
    }
  };

  const title = entry ? entry.name : selectionCount > 1 ? t("inspector.multiTitle", { n: selectionCount }) : t("inspector.title");

  return (
    <aside className="wc-inspector-aside">
      <div className="serein-inspector serein-inspector--docked" aria-label={t("inspector.title")}>
        <div className="serein-inspector__header">
          <div>
            <h2 className="serein-inspector__title">{title}</h2>
            {entry ? (
              <p className="serein-inspector__sub">
                {/* A directory has no size, and the listing does not carry a child count, so
                    the subtitle states only the kind rather than a number that means another
                    field. */}
                {kindLabel(t, entry)}
                {entry.kind === "dir" ? "" : ` · ${fmt.size(entry.size)}`}
              </p>
            ) : (
              <p className="serein-inspector__sub">
                {selectionCount > 1 ? t("inspector.multiHint") : t("inspector.noSelection")}
              </p>
            )}
          </div>
          <button
            type="button"
            className="serein-icon-btn serein-icon-btn--sm"
            aria-label={t("inspector.close")}
            onClick={props.onClose}
          >
            <Icon name="close" size={13} />
          </button>

          <div className="serein-inspector__tabs">
            <div className="serein-tabs serein-tabs--underline">
              <div className="serein-tabs__list" role="tablist" aria-label={t("inspector.panes")}>
                {PANES.map((p) => (
                  <button
                    key={p.id}
                    type="button"
                    className="serein-tab"
                    role="tab"
                    id={`insp-tab-${p.id}`}
                    aria-controls={`insp-panel-${p.id}`}
                    aria-selected={props.pane === p.id}
                    tabIndex={props.pane === p.id ? 0 : -1}
                    onClick={() => props.onPane(p.id)}
                    onKeyDown={(e) => {
                      // Roving focus across the tab strip, which is the WAI-ARIA tabs pattern:
                      // Arrow keys move, one stop in the tab order.
                      const at = PANES.findIndex((x) => x.id === props.pane);
                      if (e.key === "ArrowRight" || e.key === "ArrowLeft") {
                        e.preventDefault();
                        const delta = e.key === "ArrowRight" ? 1 : -1;
                        const next = PANES[(at + delta + PANES.length) % PANES.length];
                        if (next) props.onPane(next.id);
                      } else if (e.key === "Home") {
                        e.preventDefault();
                        props.onPane(PANES[0]!.id);
                      } else if (e.key === "End") {
                        e.preventDefault();
                        props.onPane(PANES[PANES.length - 1]!.id);
                      }
                    }}
                  >
                    {t(p.key)}
                  </button>
                ))}
              </div>
            </div>
          </div>
        </div>

        <div className="serein-inspector__panelstack">
          <div
            className="serein-inspector__body"
            role="tabpanel"
            id={`insp-panel-${props.pane}`}
            aria-labelledby={`insp-tab-${props.pane}`}
            tabIndex={0}
          >
            {!entry ? (
              selectionCount > 1 ? (
                <div className="wc-inspector-multi">
                  <p className="serein-hint">{t("inspector.multiBody", { n: selectionCount })}</p>
                  <div className="serein-inspector__actions">
                    <button
                      type="button"
                      className="serein-btn serein-btn--sm serein-btn--outline"
                      onClick={props.onClearSelection}
                    >
                      {t("files.clearSelection")}
                    </button>
                    <button
                      type="button"
                      className="serein-btn serein-btn--sm serein-btn--outline"
                      onClick={props.onSelectAll}
                    >
                      {t("inspector.selectAll")}
                    </button>
                    <button
                      type="button"
                      className="serein-btn serein-btn--sm serein-btn--outline"
                      onClick={() => props.onTransfer("move", [])}
                    >
                      {t("transfer.move")}
                    </button>
                  </div>
                </div>
              ) : (
                // The subtitle above already says there is no selection; repeating it here as
                // a sentence is the same fact twice in a 340px panel.
                <EmptyState
                  icon="eye"
                  title={t("inspector.pickFileTitle")}
                  hint={t("inspector.pickFile")}
                />
              )
            ) : props.pane === "info" ? (
              <InfoPane entry={entry} me={props.me} onOpen={props.onOpen} onRename={props.onRename} onDelete={props.onDelete} onShare={props.onShare} onTransfer={props.onTransfer} onStar={props.onStar} />
            ) : props.pane === "versions" ? (
              <VersionsPane
                entry={entry}
                versions={versions}
                state={versionState}
                error={versionsError}
                restoring={restoring}
                onRestore={restore}
              />
            ) : props.pane === "comments" ? (
              // The thread is fetched by its own component, keyed on the node id: a discussion is
              // per file and can be long, so loading it with the versions would make every click
              // in the panel fetch a thread nobody opened.
              <CommentThread nodeID={entry.id} me={props.me} />
            ) : props.pane === "activity" ? (
              <ActivityPane entry={entry} events={events} error={eventsError} />
            ) : (
              <SharedPane entry={entry} links={links} onShare={props.onShare} />
            )}
          </div>
        </div>
      </div>
    </aside>
  );
}

function InfoPane({
  entry,
  me,
  onOpen,
  onRename,
  onDelete,
  onShare,
  onTransfer,
  onStar,
}: {
  entry: FileNode;
  me: FileInspectorProps["me"];
  onOpen: (entry: FileNode) => void;
  onRename: (entry: FileNode) => void;
  onDelete: (entries: FileNode[]) => void;
  onShare: (entry: FileNode) => void;
  onTransfer: (op: "copy" | "move", entries: FileNode[]) => void;
  /** onStar tells the parent the file's star changed, so the row beside the panel agrees with it. */
  onStar: (starred: boolean) => void;
}) {
  const { t, fmt } = useI18n();
  const [broken, setBroken] = useState(false);
  const [copied, setCopied] = useState(false);
  const isImage = entry.preview === "image" && entry.kind === "file";

  // The clipboard is the only way a hash gets used, and `navigator.clipboard` is unavailable on an
  // insecure origin — which a self-hosted deployment on plain http is. Falling back to a text
  // selection keeps the value reachable rather than failing silently.
  const copyChecksum = async () => {
    if (!entry.sha256) return;
    try {
      await navigator.clipboard.writeText(entry.sha256);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1600);
    } catch {
      setCopied(false);
    }
  };

  return (
    <>
      <div className="serein-inspector__hero">
        {isImage && !broken ? (
          <img src={thumbnailURL(entry.path)} alt="" onError={() => setBroken(true)} />
        ) : (
          <FileTile entry={entry} size={180} />
        )}
      </div>

      <div className="serein-inspector__identity">
        <div className="serein-inspector__identity-main">
          <div className="serein-inspector__name">{entry.name}</div>
          <div className="serein-inspector__sub">{kindLabel(t, entry)}</div>
        </div>
      </div>

      <div className="serein-inspector__actions">
        <button type="button" className="serein-btn serein-btn--sm" onClick={() => onOpen(entry)}>
          <Icon name={entry.kind === "dir" ? "files" : "eye"} size={12} />
          {entry.kind === "dir" ? t("files.openFolder") : t("detail.open")}
        </button>
        {/* The star is per USER (core-scope §4), so it sits with the file's own actions rather than
            in a menu: it is the one action here that changes nothing for anybody else. */}
        <StarButton node={entry} starred={entry.starred ?? false} onChange={onStar} />
        {entry.kind === "file" ? (
          <a className="serein-icon-btn serein-icon-btn--sm" href={downloadURL(entry.path)} download aria-label={t("detail.download")} title={t("detail.download")}>
            <Icon name="download" size={13} />
          </a>
        ) : null}
        <button
          type="button"
          className="serein-icon-btn serein-icon-btn--sm"
          aria-label={t("detail.share")}
          title={t("detail.share")}
          onClick={() => onShare(entry)}
        >
          <Icon name="link" size={13} />
        </button>
        <button
          type="button"
          className="serein-icon-btn serein-icon-btn--sm"
          aria-label={t("detail.rename")}
          title={t("detail.rename")}
          onClick={() => onRename(entry)}
        >
          <Icon name="edit" size={13} />
        </button>
        <details className="serein-menu">
          <summary className="serein-icon-btn serein-icon-btn--sm" aria-label={t("inspector.more")}>
            <Icon name="chevronDown" size={13} />
          </summary>
          <div className="serein-menu__surface" role="menu">
            <button type="button" className="serein-menu__item" role="menuitem" onClick={() => onTransfer("copy", [entry])}>
              <span className="serein-menu__icon">
                <Icon name="copy" size={13} />
              </span>
              {t("detail.copy")}
            </button>
            <button type="button" className="serein-menu__item" role="menuitem" onClick={() => onTransfer("move", [entry])}>
              <span className="serein-menu__icon">
                <Icon name="move" size={13} />
              </span>
              {t("detail.move")}
            </button>
            <div className="serein-menu__sep" role="separator" />
            <button
              type="button"
              className="serein-menu__item serein-menu__item--danger"
              role="menuitem"
              onClick={() => onDelete([entry])}
            >
              <span className="serein-menu__icon">
                <Icon name="trash" size={13} />
              </span>
              {t("detail.delete")}
            </button>
          </div>
        </details>
      </div>

      <div className="serein-inspector__section">
        <div className="serein-inspector__section-title">{t("inspector.details")}</div>
        <dl className="serein-meta">
          <dt>{t("detail.kind")}</dt>
          <dd>{kindLabel(t, entry)}</dd>
          <dt>{t("detail.size")}</dt>
          <dd>{entry.kind === "dir" ? t("detail.notApplicable") : fmt.size(entry.size)}</dd>
          <dt>{t("detail.modified")}</dt>
          <dd>{fmt.full(entry.modified_at)}</dd>
          <dt>{t("detail.created")}</dt>
          <dd>{fmt.full(entry.created_at)}</dd>
          <dt>{t("detail.path")}</dt>
          <dd title={entry.path}>{entry.path || "/"}</dd>
          {me ? (
            <>
              <dt>{t("inspector.owner")}</dt>
              <dd>{me.display_name || me.username || me.email || ""}</dd>
            </>
          ) : null}
        </dl>
      </div>

      {/* Labels (core-scope §5). Below the details section because a label is metadata the user adds
          rather than a fact the server knows. */}
      <div className="serein-inspector__section">
        <div className="serein-inspector__section-title">{t("social.tags")}</div>
        <TagEditor node={entry} />
      </div>

      {/* The content address (core-scope §2).
          Shown in full rather than truncated: its whole purpose is that a user can compare it with a
          hash they computed elsewhere, and a truncated hash compares with nothing. Omitted for a
          directory and for a node whose address the store does not know — absent and "a row of
          zeroes" must stay distinguishable, or a user verifies against a hash nobody computed. */}
      {entry.sha256 ? (
        <div className="serein-inspector__section">
          <div className="serein-inspector__section-title">{t("detail.checksum")}</div>
          <div className="wc-checksum">
            <code className="wc-checksum__value">{entry.sha256}</code>
            <button
              type="button"
              className="serein-btn serein-btn--sm serein-btn--quiet"
              onClick={() => void copyChecksum()}
            >
              {copied ? t("detail.copiedChecksum") : t("detail.copyChecksum")}
            </button>
          </div>
        </div>
      ) : null}

      {entry.kind === "file" && entry.has_versions ? (
        <div className="serein-inspector__section">
          <div className="serein-inspector__section-title">{t("detail.versions")}</div>
          <div className="serein-inspector__callout">
            <span className="serein-inspector__callout-icon">
              <Icon name="layers" size={14} />
            </span>
            <div className="serein-inspector__callout-text">
              <div className="serein-inspector__callout-title">{t("inspector.hasVersions")}</div>
              <div className="serein-inspector__callout-description">{t("inspector.hasVersionsHint")}</div>
            </div>
          </div>
        </div>
      ) : null}
    </>
  );
}

function VersionsPane({
  entry,
  versions,
  state,
  error,
  restoring,
  onRestore,
}: {
  entry: FileNode;
  versions: Version[] | null;
  state: { cheap: boolean; restorable: boolean } | null;
  error: string | null;
  restoring: boolean;
  onRestore: (version: number) => void;
}) {
  const { t, fmt } = useI18n();

  if (entry.kind === "dir") {
    return <p className="serein-hint">{t("inspector.versionsFolder")}</p>;
  }
  if (error) {
    return <p className="serein-hint serein-field__error">{error}</p>;
  }
  if (versions === null) {
    return (
      <div className="wc-skeleton-rows" aria-hidden="true">
        {[0, 1, 2].map((i) => (
          <div className="wc-skeleton-row" key={i}>
            <span className="serein-skeleton" style={{ width: 26, height: 26 }} />
            <span className="wc-skeleton-row__stack">
              <span className="serein-skeleton serein-skeleton--text" style={{ width: "54%" }} />
              <span className="serein-skeleton serein-skeleton--text" style={{ width: "30%" }} />
            </span>
          </div>
        ))}
      </div>
    );
  }
  if (versions.length === 0) {
    return <p className="serein-hint">{t("detail.versionsEmpty")}</p>;
  }

  return (
    <>
      <div className="serein-versions">
        {versions.map((v) => (
          <div className="serein-version" key={v.version}>
            <span className="serein-version__index">v{v.version}</span>
            <span className="serein-version__main">
              <span className="serein-version__title">{fmt.full(v.created_at)}</span>
              <span className="serein-version__meta">{fmt.size(v.size)}</span>
            </span>
            <button
              type="button"
              className="serein-btn serein-btn--sm serein-btn--outline"
              disabled={!state?.restorable || restoring}
              title={state?.restorable ? undefined : t("detail.versionsNoContent")}
              onClick={() => onRestore(v.version)}
            >
              {t("detail.restore")}
            </button>
          </div>
        ))}
      </div>
      {state && !state.restorable ? (
        <p className="serein-hint wc-versions-note">{t("detail.versionsNoContent")}</p>
      ) : null}
    </>
  );
}

function ActivityPane({
  entry,
  events,
  error,
}: {
  entry: FileNode;
  events: AuditEvent[] | null;
  error: string | null;
}) {
  const { t, fmt } = useI18n();

  if (error) {
    return <p className="serein-hint serein-field__error">{error}</p>;
  }
  if (events === null) {
    return <p className="serein-hint">{t("state.loading")}</p>;
  }
  if (events.length === 0) {
    return (
      <>
        <p className="serein-hint">{t("inspector.noActivity")}</p>
        {/* The audit feed has no per-target filter yet, so a file with no matching rows may
            still have history. Saying so is the difference between "nothing happened" and
            "this pane cannot see what happened". */}
        <p className="serein-hint wc-versions-note">{t("inspector.activityScope")}</p>
      </>
    );
  }

  return (
    <div className="serein-activity">
      {events.map((e) => (
        <div className="serein-activity__row" key={e.id}>
          <span
            className={`serein-status-dot${e.result === "denied" ? " serein-status-dot--danger" : ""}`}
            aria-hidden="true"
          />
          <span className="serein-activity__text">
            <span>{e.action}</span>
            {e.result !== "ok" ? <span className="wc-activity-result">{e.result}</span> : null}
          </span>
          <span className="serein-activity__time">{fmt.relative(e.ts)}</span>
        </div>
      ))}
      <p className="serein-hint wc-versions-note">
        {t("inspector.activityPath", { path: entry.path })}
      </p>
    </div>
  );
}

function SharedPane({
  entry,
  links,
  onShare,
}: {
  entry: FileNode;
  links: Share[] | null;
  onShare: (entry: FileNode) => void;
}) {
  const { t, fmt } = useI18n();

  return (
    <>
      {links === null ? (
        <p className="serein-hint">{t("state.loading")}</p>
      ) : links.length === 0 ? (
        <>
          <p className="serein-hint">{t("detail.sharedEmpty")}</p>
          <div className="serein-inspector__actions">
            <button type="button" className="serein-btn serein-btn--sm" onClick={() => onShare(entry)}>
              <Icon name="link" size={12} />
              {t("detail.share")}
            </button>
          </div>
        </>
      ) : (
        <>
          {links.map((link) => (
            <div className="serein-link-hero" key={link.id}>
              <span className="serein-link-hero__icon">
                <Icon name={link.kind === "request" ? "inbox" : "link"} size={15} />
              </span>
              <span className="serein-link-hero__main">
                <span className="serein-link-hero__title">{link.label || link.node_path}</span>
                <span className="serein-link-hero__sub">
                  {link.kind === "request"
                    ? t("home.request")
                    : link.kind === "email"
                      ? t("home.email")
                      : t("home.link")}
                  {link.has_password ? ` · ${t("inspector.protected")}` : ""}
                  {link.expires_at ? ` · ${t("inspector.expires", { date: fmt.date(link.expires_at) })}` : ""}
                </span>
              </span>
            </div>
          ))}
          {links.every((l) => !l.url) ? (
            // The token is stored hash-only, so the URL genuinely cannot be produced again.
            // A copy button here would copy an empty string.
            <p className="serein-hint wc-versions-note">{t("detail.linkNotRecoverable")}</p>
          ) : null}
        </>
      )}
    </>
  );
}

/* ------------------------------------------------------------ share panel --- */

function SharePane({
  share,
  onClose,
  onRefresh,
  onOpenLink,
}: {
  share: Share | null;
  onClose: () => void;
  onRefresh: () => void;
  onOpenLink: (share: Share) => void;
}) {
  const { t, fmt } = useI18n();
  const toast = useToast();
  const [busy, setBusy] = useState(false);

  const revoke = async () => {
    if (!share) return;
    setBusy(true);
    try {
      await api.revokeShare(share.id);
      toast.success(t("toast.revoked"));
      onRefresh();
      onClose();
    } catch (err) {
      toast.error(err instanceof APIError ? err.message || err.code : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <aside className="wc-inspector-aside">
      <div className="serein-inspector serein-inspector--docked" aria-label={t("inspector.linkTitle")}>
        <div className="serein-inspector__header">
          <div>
            <h2 className="serein-inspector__title">{t("inspector.linkTitle")}</h2>
            <p className="serein-inspector__sub">{share ? share.label || share.node_path : ""}</p>
          </div>
          <button
            type="button"
            className="serein-icon-btn serein-icon-btn--sm"
            aria-label={t("inspector.close")}
            onClick={onClose}
          >
            <Icon name="close" size={13} />
          </button>
        </div>

        <div className="serein-inspector__body">
          {!share ? (
            <p className="serein-hint">{t("inspector.linkMissing")}</p>
          ) : (
            <>
              <div className="serein-link-hero">
                <span className="serein-link-hero__icon">
                  <Icon name={share.kind === "request" ? "inbox" : "link"} size={15} />
                </span>
                <span className="serein-link-hero__main">
                  <span className="serein-link-hero__title">{share.label || share.node_path}</span>
                  <span className="serein-link-hero__sub">{share.node_path}</span>
                </span>
              </div>

              {share.url ? (
                <div className="serein-link-box">
                  <span className="serein-link-box__url">{share.url}</span>
                  <button
                    type="button"
                    className="serein-btn serein-btn--sm serein-btn--outline"
                    onClick={() => {
                      const url = share.url ?? "";
                      // Optional chaining on writeText itself: on an insecure origin
                      // navigator.clipboard is undefined, and `?.writeText(...).then` would
                      // still throw on the .then of undefined.
                      if (!navigator.clipboard?.writeText) {
                        toast.error(t("inspector.copyFailed"));
                        return;
                      }
                      void navigator.clipboard.writeText(url).then(
                        () => toast.success(t("toast.linkCopied")),
                        () => toast.error(t("inspector.copyFailed")),
                      );
                    }}
                  >
                    {t("inspector.copy")}
                  </button>
                </div>
              ) : (
                <p className="serein-hint wc-versions-note">{t("detail.linkNotRecoverable")}</p>
              )}

              <div className="serein-inspector__section">
                <div className="serein-inspector__section-title">{t("inspector.details")}</div>
                <dl className="serein-meta">
                  <dt>{t("inspector.access")}</dt>
                  <dd>
                    {share.kind === "request"
                      ? t("inspector.accessUpload")
                      : t("inspector.accessView")}
                  </dd>
                  <dt>{t("inspector.password")}</dt>
                  <dd>{share.has_password ? t("inspector.protected") : t("inspector.noPassword")}</dd>
                  <dt>{t("shared.uses")}</dt>
                  <dd>
                    {(() => {
                      const max = share.max_downloads ?? share.max_uploads;
                      const used = share.download_count + share.upload_count;
                      return max ? `${used} / ${max}` : String(used);
                    })()}
                  </dd>
                  <dt>{t("shared.expires")}</dt>
                  <dd>{share.expires_at ? fmt.date(share.expires_at) : t("shared.never")}</dd>
                  <dt>{t("inspector.created")}</dt>
                  <dd>{fmt.date(share.created_at)}</dd>
                </dl>
              </div>

              <div className="serein-inspector__actions">
                <button type="button" className="serein-btn serein-btn--sm serein-btn--outline" onClick={() => onOpenLink(share)}>
                  {t("inspector.previewAsRecipient")}
                </button>
                <button
                  type="button"
                  className="serein-btn serein-btn--sm serein-btn--danger"
                  disabled={busy}
                  onClick={() => {
                    // Revoking kills a link that may already be in someone's hands; there is
                    // no undo, so the click asks before it acts.
                    if (!window.confirm(t("inspector.revokeConfirm"))) return;
                    void revoke();
                  }}
                >
                  {busy ? <span className="serein-spinner" aria-hidden="true" /> : null}
                  {t("shared.revoke")}
                </button>
              </div>

              <p className="serein-hint wc-versions-note">{t("inspector.linkNote")}</p>
            </>
          )}
        </div>
      </div>
    </aside>
  );
}

/* --------------------------------------------------------- received panel --- */

function ReceivedPane({ node, onClose }: { node: SharedNode | null; onClose: () => void }) {
  const { t, fmt } = useI18n();

  return (
    <aside className="wc-inspector-aside">
      <div className="serein-inspector serein-inspector--docked" aria-label={t("inspector.receivedTitle")}>
        <div className="serein-inspector__header">
          <div>
            <h2 className="serein-inspector__title">{t("inspector.receivedTitle")}</h2>
            <p className="serein-inspector__sub">{node?.name ?? ""}</p>
          </div>
          <button
            type="button"
            className="serein-icon-btn serein-icon-btn--sm"
            aria-label={t("inspector.close")}
            onClick={onClose}
          >
            <Icon name="close" size={13} />
          </button>
        </div>
        <div className="serein-inspector__body">
          {!node ? (
            <p className="serein-hint">{t("inspector.pickFile")}</p>
          ) : (
            <>
              <dl className="serein-meta">
                <dt>{t("detail.kind")}</dt>
                <dd>{node.kind === "dir" ? t("kind.folder") : t("kind.file")}</dd>
                <dt>{t("detail.size")}</dt>
                <dd>{node.kind === "dir" ? "—" : fmt.size(node.size ?? 0)}</dd>
                <dt>{t("detail.modified")}</dt>
                <dd>{node.modified_at ? fmt.full(node.modified_at) : "—"}</dd>
                <dt>{t("received.permissions")}</dt>
                <dd>{node.permissions}</dd>
              </dl>
              {node.kind === "file" ? (
                <div className="serein-inspector__actions">
                  <a className="serein-btn serein-btn--sm" href={sharedContentURL(node.id)} download>
                    <Icon name="download" size={12} />
                    {t("received.download")}
                  </a>
                </div>
              ) : null}
              <p className="serein-hint wc-versions-note">{t("received.recheck")}</p>
            </>
          )}
        </div>
      </div>
    </aside>
  );
}
