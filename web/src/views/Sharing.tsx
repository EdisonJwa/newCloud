// Sharing — two directions, never merged.
//
// The single worst information-architecture defect in the version this replaces was one screen
// called "Shared" that was, in fact, the outbound link manager. Home's "Shared with you" link
// pointed at it, so the link promised received files and delivered public links. A suite that
// only asserted "clicking Shared opens Shared" could not see it.
//
// So there is one destination with two labelled views, and the view is in the URL:
//
//   Shared with me   inbound files from other accounts; Name · Shared by · Access
//   Links I manage   outbound links this account created; link · access · usage · expiry
//
// **This is the only application I have seen where the received side is the default**, and it
// is the right default here: somebody arriving at "Shared" without context is asking what
// other people sent them. The outbound list is a management surface, which is a thing you go
// to deliberately.
//
// ## The received side is read-only, and says so
//
// Access and sender are shown accurately, and the raw permission string is available in the
// Inspector for anyone who wants to know why an action is missing. There is no "remove from my
// list" and no permission request: neither has a server contract, and a button that produces
// an error is worse than the sentence explaining its absence.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { api, APIError, sharedContentURL } from "../api";
import { useI18n } from "../i18n";
import { EmptyState, ErrorState, FileTile, Icon, Skeleton, useToast } from "../ui";
import { Inspector } from "./Inspector";
import { accessLabel, previewOf } from "../nodes";
import type { Share, SharedNode } from "../types";
import type { Route } from "../route";

interface Props {
  route: Route;
  reloadKey: number;
  onRoute: (changes: Partial<Route>, mode: "push" | "replace") => void;
  onOpenReceived: (node: SharedNode) => void;
  onOpenSharedFile: (node: SharedNode) => void;
  /** Opens the public recipient page of a link, which is a real external URL. */
  onPreviewLink: (share: Share) => void;
}

export function Sharing({ route, reloadKey, onRoute, onOpenReceived, onOpenSharedFile, onPreviewLink }: Props) {
  const { t } = useI18n();

  return (
    <div className="serein-page serein-page--sharing">
      <div className="serein-page-header">
        <div className="serein-page-header__main">
          <h1 className="serein-page-header__title">{t("shared.title")}</h1>
          <p className="serein-page-header__description">
            {route.tab === "received" ? t("shared.subtitleReceived") : t("shared.subtitle")}
          </p>
        </div>
      </div>

      <div data-serein-tabs>
        <div className="serein-tabs serein-tabs--underline wc-share-tabs">
          <div className="serein-tabs__list" role="tablist" aria-label={t("shared.tabs")}>
            <button
              type="button"
              className="serein-tab"
              role="tab"
              id="share-tab-received"
              aria-controls="share-panel-received"
              aria-selected={route.tab === "received"}
              tabIndex={route.tab === "received" ? 0 : -1}
              onClick={() => onRoute({ tab: "received", share: "", inspector: false }, "push")}
            >
              <span className="serein-tab__icon">
                <Icon name="inbox" size={12} />
              </span>
              {t("shared.tabReceived")}
            </button>
            <button
              type="button"
              className="serein-tab"
              role="tab"
              id="share-tab-managed"
              aria-controls="share-panel-managed"
              aria-selected={route.tab === "managed"}
              tabIndex={route.tab === "managed" ? 0 : -1}
              onClick={() => onRoute({ tab: "managed", share: "", inspector: false }, "push")}
            >
              <span className="serein-tab__icon">
                <Icon name="link" size={12} />
              </span>
              {t("shared.tabLinks")}
            </button>
          </div>
        </div>

        {route.tab === "received" ? (
          <div
            className="serein-tabpanel"
            role="tabpanel"
            id="share-panel-received"
            aria-labelledby="share-tab-received"
            tabIndex={0}
          >
            <Received
              reloadKey={reloadKey}
              onOpen={onOpenReceived}
              onOpenFile={onOpenSharedFile}
              onRoute={onRoute}
              route={route}
            />
          </div>
        ) : (
          <div
            className="serein-tabpanel"
            role="tabpanel"
            id="share-panel-managed"
            aria-labelledby="share-tab-managed"
            tabIndex={0}
          >
            <Managed
              reloadKey={reloadKey}
              onRoute={onRoute}
              route={route}
              onPreviewLink={onPreviewLink}
            />
          </div>
        )}
      </div>
    </div>
  );
}

/* --------------------------------------------------------------- received --- */

function Received({
  reloadKey,
  onOpen,
  onOpenFile,
  route,
  onRoute,
}: {
  reloadKey: number;
  onOpen: (node: SharedNode) => void;
  onOpenFile: (node: SharedNode) => void;
  route: Route;
  onRoute: Props["onRoute"];
}) {
  const { t, fmt } = useI18n();
  const toast = useToast();
  const [items, setItems] = useState<SharedNode[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [expanded, setExpanded] = useState<Record<string, SharedNode["children"] | null>>({});
  const [busy, setBusy] = useState<string | null>(null);
  const [selected, setSelected] = useState<SharedNode | null>(null);

  const load = useCallback(() => {
    setItems(null);
    setError(null);
    setExpanded({});
    api
      .sharedWithMe()
      .then((res) => setItems(res.items ?? []))
      .catch((err) => setError(err instanceof APIError ? err.message || err.code : String(err)));
  }, []);

  useEffect(load, [load, reloadKey]);

  const toggle = async (node: SharedNode) => {
    if (node.id in expanded) {
      setExpanded((prev) => {
        const next = { ...prev };
        delete next[node.id];
        return next;
      });
      return;
    }
    setBusy(node.id);
    try {
      const full = await api.sharedNode(node.id);
      setExpanded((prev) => ({ ...prev, [node.id]: full.children ?? [] }));
    } catch (err) {
      toast.error(err instanceof APIError ? err.message || err.code : String(err));
      setExpanded((prev) => ({ ...prev, [node.id]: null }));
    } finally {
      setBusy(null);
    }
  };

  if (error) return <ErrorState message={error} onRetry={load} />;
  if (items === null) return <Skeleton rows={5} />;
  if (items.length === 0) {
    return <EmptyState icon="shares" title={t("received.empty")} hint={t("received.emptyHint")} />;
  }

  return (
    <div className="wc-split" data-inspector={route.inspector ? "docked" : "closed"}>
      <div>
        <div className="wc-card">
          <div className="serein-share-list">
            {items.map((node) => (
              <ReceivedRow
                key={node.id}
                node={node}
                depth={0}
                expanded={expanded}
                busy={busy}
                selected={selected?.id === node.id}
                onSelect={() => {
                  setSelected(node);
                  if (!route.inspector) onRoute({ inspector: true }, "push");
                }}
                onToggle={() => void toggle(node)}
                onOpen={() => onOpen(node)}
                onOpenFile={() => onOpenFile(node)}
                fmt={fmt}
                t={t}
              />
            ))}
          </div>
        </div>
        {/* The permission shown is the authorizer's answer for the ROOT of each granted
            subtree. Children are re-authorized when opened, so a download can still be
            refused; saying so is more honest than implying the list is a permission map. */}
        <p className="serein-hint wc-share-note">{t("received.recheck")}</p>
      </div>

      {route.inspector ? (
        <Inspector
          mode="received"
          node={selected}
          onClose={() => onRoute({ inspector: false }, "push")}
        />
      ) : null}
    </div>
  );
}

function ReceivedRow({
  node,
  depth,
  expanded,
  busy,
  selected,
  onSelect,
  onToggle,
  onOpen,
  onOpenFile,
  fmt,
  t,
}: {
  node: SharedNode;
  depth: number;
  expanded: Record<string, SharedNode["children"] | null>;
  busy: string | null;
  selected: boolean;
  onSelect: () => void;
  onToggle: () => void;
  onOpen: () => void;
  onOpenFile: () => void;
  fmt: { size: (bytes: number) => string; relative: (iso: string) => string };
  t: (key: string, params?: Record<string, string | number>) => string;
}) {
  const isDir = node.kind === "dir";
  const open = node.id in expanded;
  const children = expanded[node.id];

  return (
    <>
      <div
        className="serein-share-row wc-received-row"
        data-selected={selected}
        style={depth > 0 ? { paddingInlineStart: 14 + depth * 18 } : undefined}
        onClick={onSelect}
      >
        <FileTile
          entry={{
            kind: isDir ? "dir" : "file",
            preview: previewOf(node.mime, node.name),
            name: node.name,
            path: "",
          }}
          size={34}
        />

        <div className="serein-share-row__main">
          <span className="serein-share-row__title">{node.name}</span>
          <span className="serein-share-row__meta">
            {isDir ? t("kind.folder") : fmt.size(node.size ?? 0)}
            {node.modified_at ? ` · ${fmt.relative(node.modified_at)}` : ""}
          </span>
        </div>

        <span className="serein-share-row__access" title={node.permissions}>
          {accessLabel(t, node.permissions)}
        </span>

        <span className="serein-share-row__owner">
          <span className="serein-avatar serein-avatar--sm" aria-hidden="true">
            {initialsFrom(node.owner_id ?? "")}
          </span>
          <span className="serein-share-row__owner-name">{shortOwner(node.owner_id ?? "")}</span>
        </span>

        <span className="serein-share-row__expiry" />

        <div className="wc-received-actions">
          {isDir ? (
            <>
              <button
                type="button"
                className="serein-btn serein-btn--sm serein-btn--quiet"
                disabled={busy === node.id}
                aria-expanded={open}
                onClick={(e) => {
                  e.stopPropagation();
                  onToggle();
                }}
              >
                {open ? t("received.close") : t("received.open")}
              </button>
              <button
                type="button"
                className="serein-icon-btn serein-icon-btn--sm"
                aria-label={t("received.openInFiles")}
                title={t("received.openInFiles")}
                onClick={(e) => {
                  e.stopPropagation();
                  onOpen();
                }}
              >
                <Icon name="arrowLeft" size={12} />
              </button>
            </>
          ) : (
            <>
              <button
                type="button"
                className="serein-btn serein-btn--sm serein-btn--quiet"
                onClick={(e) => {
                  e.stopPropagation();
                  onOpenFile();
                }}
              >
                {t("received.open")}
              </button>
              <a
                className="serein-icon-btn serein-icon-btn--sm"
                href={sharedContentURL(node.id)}
                download
                aria-label={t("share.downloadNamed", { name: node.name })}
                title={t("received.download")}
                onClick={(e) => e.stopPropagation()}
              >
                <Icon name="download" size={12} />
              </a>
            </>
          )}
        </div>
      </div>

      {open && children === null ? (
        <p className="serein-hint wc-received-empty" style={{ paddingInlineStart: 14 + (depth + 1) * 18 }}>
          {t("received.unavailable")}
        </p>
      ) : null}

      {open && children
        ? children.length === 0
          ? (
            <p className="serein-hint wc-received-empty" style={{ paddingInlineStart: 14 + (depth + 1) * 18 }}>
              {t("received.noChildren")}
            </p>
          )
          : children.map((child) => (
            <ReceivedChild key={child.id} child={child} depth={depth + 1} fmt={fmt} t={t} />
          ))
        : null}
    </>
  );
}

/** ReceivedChild is the shallow version of a received row: children carry no owner or
 *  permission string, because the server re-authorizes each one on access. */
function ReceivedChild({
  child,
  depth,
  fmt,
  t,
}: {
  child: { id: string; name: string; kind: string; size?: number | null };
  depth: number;
  fmt: { size: (bytes: number) => string };
  t: (key: string, params?: Record<string, string | number>) => string;
}) {
  return (
    <div className="serein-share-row wc-received-row" style={{ paddingInlineStart: 14 + depth * 18 }}>
      <FileTile
        entry={{
          kind: child.kind === "dir" ? "dir" : "file",
          preview: undefined,
          name: child.name,
          path: "",
        }}
        size={30}
      />
      <div className="serein-share-row__main">
        <span className="serein-share-row__title">{child.name}</span>
        <span className="serein-share-row__meta">
          {child.kind === "dir" ? t("kind.folder") : fmt.size(child.size ?? 0)}
        </span>
      </div>
      <span className="serein-share-row__access" />
      <span className="serein-share-row__owner" />
      <span className="serein-share-row__expiry" />
      <div className="wc-received-actions">
        {child.kind === "file" ? (
          <a
            className="serein-icon-btn serein-icon-btn--sm"
            href={sharedContentURL(child.id)}
            download
            aria-label={t("share.downloadNamed", { name: child.name })}
          >
            <Icon name="download" size={12} />
          </a>
        ) : null}
      </div>
    </div>
  );
}

/* ---------------------------------------------------------------- managed --- */

function Managed({
  reloadKey,
  route,
  onRoute,
  onPreviewLink,
}: {
  reloadKey: number;
  route: Route;
  onRoute: Props["onRoute"];
  onPreviewLink: (share: Share) => void;
}) {
  const { t, fmt } = useI18n();
  const toast = useToast();
  const [items, setItems] = useState<Share[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [confirmRevoke, setConfirmRevoke] = useState<Share | null>(null);

  const load = useCallback(() => {
    setItems(null);
    setError(null);
    api
      .shares()
      .then((res) => setItems((res.items ?? []).filter((s) => !s.revoked_at)))
      .catch((err) => setError(err instanceof APIError ? err.message || err.code : String(err)));
  }, []);

  useEffect(load, [load, reloadKey]);

  // Three buckets rather than two: a link that has run out of uses or expired is not "active",
  // but deleting it from the list would hide the evidence of what was shared.
  const buckets = useMemo(() => {
    const now = Date.now();
    const list = items ?? [];
    return {
      active: list.filter((s) => !isSpent(s, now)),
      spent: list.filter((s) => isSpent(s, now)),
    };
  }, [items]);

  const revoke = async (share: Share) => {
    try {
      await api.revokeShare(share.id);
      toast.success(t("toast.revoked"));
      setConfirmRevoke(null);
      load();
      if (route.share === share.id) onRoute({ share: "", inspector: false }, "replace");
    } catch (err) {
      toast.error(err instanceof APIError ? err.message || err.code : String(err));
    }
  };

  const selected = items?.find((s) => s.id === route.share) ?? null;

  return (
    <div className="wc-split" data-inspector={route.inspector && selected ? "docked" : "closed"}>
      <div>
        <div className="wc-share-toolbar">
          <p className="serein-hint">{t("shared.managedHint")}</p>
          <button
            type="button"
            className="serein-btn serein-btn--outline"
            onClick={() => onRoute({ screen: "files" }, "push")}
          >
            <Icon name="folderPlus" size={13} />
            <span className="serein-btn__label">{t("shared.pickFileToShare")}</span>
          </button>
        </div>

        {error ? (
          <ErrorState message={error} onRetry={load} />
        ) : items === null ? (
          <Skeleton rows={4} />
        ) : items.length === 0 ? (
          <EmptyState
            icon="link"
            title={t("shared.empty")}
            hint={t("shared.emptyHint")}
            actions={
              <button type="button" className="serein-btn" onClick={() => onRoute({ screen: "files" }, "push")}>
                {t("home.browse")}
              </button>
            }
          />
        ) : (
          <>
            <div className="serein-share-list wc-card wc-card--flush">
              <div className="wc-sharehead" aria-hidden="true">
                <span />
                <span>{t("shared.colLink")}</span>
                <span>{t("shared.colAccess")}</span>
                <span>{t("shared.colUsage")}</span>
                <span>{t("shared.colExpiry")}</span>
                <span />
              </div>
              {buckets.active.map((share) => (
                <ManagedRow
                  key={share.id}
                  share={share}
                  selected={selected?.id === share.id}
                  onSelect={() => onRoute({ share: share.id, inspector: true, pane: "share" }, "push")}
                  onPreview={() => onPreviewLink(share)}
                  onRevoke={() => setConfirmRevoke(share)}
                  fmt={fmt}
                  t={t}
                />
              ))}
            </div>

            {buckets.spent.length > 0 ? (
              <>
                <h2 className="wc-share-subhead">{t("shared.spentTitle", { n: buckets.spent.length })}</h2>
                <div className="serein-share-list wc-card wc-card--flush">
                  {buckets.spent.map((share) => (
                    <ManagedRow
                      key={share.id}
                      share={share}
                      spent
                      selected={selected?.id === share.id}
                      onSelect={() => onRoute({ share: share.id, inspector: true, pane: "share" }, "push")}
                      onPreview={() => onPreviewLink(share)}
                      onRevoke={() => setConfirmRevoke(share)}
                      fmt={fmt}
                      t={t}
                    />
                  ))}
                </div>
              </>
            ) : null}
          </>
        )}

        <p className="serein-hint wc-share-note">{t("shared.lifecycleNote")}</p>
      </div>

      {route.inspector ? (
        <Inspector
          mode="share"
          share={selected}
          onClose={() => onRoute({ share: "", inspector: false }, "replace")}
          onRefresh={load}
          onOpenLink={onPreviewLink}
        />
      ) : null}

      {confirmRevoke ? (
        <RevokeDialog
          share={confirmRevoke}
          onCancel={() => setConfirmRevoke(null)}
          onConfirm={() => void revoke(confirmRevoke)}
        />
      ) : null}
    </div>
  );
}

function ManagedRow({
  share,
  selected,
  spent,
  onSelect,
  onPreview,
  onRevoke,
  fmt,
  t,
}: {
  share: Share;
  selected: boolean;
  spent?: boolean;
  onSelect: () => void;
  onPreview: () => void;
  onRevoke: () => void;
  fmt: { date: (iso: string) => string };
  t: (key: string, params?: Record<string, string | number>) => string;
}) {
  const max = share.max_downloads ?? share.max_uploads;
  const used = share.download_count + share.upload_count;
  const percent = max ? Math.min(100, Math.round((used / max) * 100)) : 0;

  return (
    <div className="serein-share-row" data-selected={selected} onClick={onSelect}>
      <span className={`serein-inspector__callout-icon${spent ? " wc-spent-icon" : ""}`} aria-hidden="true">
        <Icon name={share.kind === "request" ? "inbox" : share.kind === "email" ? "user" : "link"} size={15} />
      </span>

      <div className="serein-share-row__main">
        <span className="serein-share-row__title">
          {share.label || share.node_path}
          {spent ? <span className="serein-badge serein-badge--warning">{t("shared.spent")}</span> : null}
        </span>
        <span className="serein-share-row__meta">{share.node_path}</span>
      </div>

      <span className="serein-share-row__access">
        {share.kind === "request" ? t("inspector.accessUpload") : t("inspector.accessView")}
        {share.has_password ? <span title={t("inspector.protected")}> · ••</span> : null}
      </span>

      <span className="serein-share-row__usage">
        <span>{max ? t("home.uses", { used, max }) : t("home.unlimited")}</span>
        {max ? (
          <span className="serein-meter" role="presentation">
            <span className="serein-meter__bar" style={{ width: `${percent}%` }} />
          </span>
        ) : null}
      </span>

      <span className="serein-share-row__expiry">
        {share.expires_at ? fmt.date(share.expires_at) : t("shared.never")}
      </span>

      <div className="wc-share-actions" onClick={(e) => e.stopPropagation()}>
        <button
          type="button"
          className="serein-icon-btn serein-icon-btn--sm"
          aria-label={t("inspector.previewAsRecipient")}
          title={t("inspector.previewAsRecipient")}
          onClick={onPreview}
        >
          <Icon name="eye" size={12} />
        </button>
        <details className="serein-menu">
          <summary className="serein-icon-btn serein-icon-btn--sm" aria-label={t("shared.rowMenu")}>
            <Icon name="chevronDown" size={13} />
          </summary>
          <div className="serein-menu__surface" role="menu">
            <button type="button" className="serein-menu__item" role="menuitem" onClick={onSelect}>
              <span className="serein-menu__icon">
                <Icon name="eye" size={13} />
              </span>
              {t("shared.openInspector")}
            </button>
            <button type="button" className="serein-menu__item" role="menuitem" onClick={onPreview}>
              <span className="serein-menu__icon">
                <Icon name="link" size={13} />
              </span>
              {t("inspector.previewAsRecipient")}
            </button>
            <div className="serein-menu__sep" role="separator" />
            <button
              type="button"
              className="serein-menu__item serein-menu__item--danger"
              role="menuitem"
              onClick={onRevoke}
            >
              <span className="serein-menu__icon">
                <Icon name="trash" size={13} />
              </span>
              {t("shared.revoke")}
            </button>
          </div>
        </details>
      </div>
    </div>
  );
}

function RevokeDialog({
  share,
  onCancel,
  onConfirm,
}: {
  share: Share;
  onCancel: () => void;
  onConfirm: () => void;
}) {
  const { t } = useI18n();
  const ref = useRef<HTMLDialogElement>(null);

  useEffect(() => {
    ref.current?.showModal();
  }, []);

  return (
    <dialog
      ref={ref}
      className="serein-dialog serein-dialog--sm"
      aria-labelledby="revoke-title"
      aria-describedby="revoke-desc"
      onCancel={(e) => {
        e.preventDefault();
        onCancel();
      }}
    >
      <div className="serein-dialog__header">
        <h2 className="serein-dialog__title" id="revoke-title">
          {t("shared.revokeTitle")}
        </h2>
        <p className="serein-dialog__description" id="revoke-desc">
          {t("shared.revokeBody", { name: share.label || share.node_path })}
        </p>
      </div>
      <div className="serein-dialog__footer">
        <button type="button" className="serein-btn serein-btn--quiet" onClick={onCancel}>
          {t("dlg.cancel")}
        </button>
        <button type="button" className="serein-btn serein-btn--danger" onClick={onConfirm}>
          {t("shared.revoke")}
        </button>
      </div>
    </dialog>
  );
}

/* ---------------------------------------------------------------- helpers --- */

/**
 * isSpent reports whether a link can no longer be used.
 *
 * A revoked link is filtered out before this runs. What is left is a link whose expiry has
 * passed or whose allowance is used up: still a fact about what was shared, which is why it is
 * shown in its own group rather than deleted.
 */
function isSpent(share: Share, now: number): boolean {
  if (share.expires_at && new Date(share.expires_at).getTime() < now) return true;
  const max = share.max_downloads ?? share.max_uploads;
  if (max === undefined || max === null) return false;
  return share.download_count + share.upload_count >= max;
}

function initialsFrom(id: string): string {
  return id.replace(/[^a-z0-9]/gi, "").slice(0, 2).toUpperCase() || "?";
}

/**
 * shortOwner abbreviates an owner id for a row.
 *
 * The API does not send a display name for the owner of a received node — only `owner_id`. The
 * alternative to showing an abbreviation is showing nothing, and a row with no sender is the
 * thing this view exists to avoid. The Inspector shows the full id, and the interface says
 * "identifier" rather than "name" so nobody reads it as a person's initials.
 */
function shortOwner(id: string): string {
  return id.slice(0, 8);
}
