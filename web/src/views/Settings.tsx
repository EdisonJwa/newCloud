// Settings — where the capabilities that are not primary destinations live.
//
// The product has four surfaces that are real and tested but are not places people work:
// Trash (undo a delete), Activity (the audit log), Access (app passwords, WebDAV, folder ACLs)
// and Accounts (an administrator's user/group/policy console). Putting them in the primary bar
// would make it a feature inventory rather than a navigation aid, and deleting them would be a
// capability regression: several of them are the only route through the UI to a server feature.
//
// So they are one destination with four tabs, reached from the account menu, and the tab is in
// the URL (`?screen=settings&settings=activity`) so a link to "Trash" opens Trash.

import { useEffect, useState } from "react";
import { api, APIError } from "../api";
import { useI18n } from "../i18n";
import { EmptyState, ErrorState, FileTile, Icon, Skeleton, useToast, kindLabel } from "../ui";
import { NewPasswordDialog } from "../components/Dialogs";
import { FolderAccess } from "../components/FolderAccess";
import { AccountAdmin } from "./Admin";
import { Activity } from "./Activity";
import type { AppPassword, FileNode, Me } from "../types";
import type { Route, SettingsTab } from "../route";

interface Props {
  route: Route;
  me: Me | null;
  reloadKey: number;
  onRoute: (changes: Partial<Route>, mode: "push" | "replace") => void;
}

const TABS: readonly { id: SettingsTab; key: string; icon: string }[] = [
  { id: "trash", key: "settings.trash", icon: "trash" },
  { id: "activity", key: "settings.activity", icon: "pulse" },
  { id: "access", key: "settings.access", icon: "key" },
  { id: "admin", key: "settings.admin", icon: "user" },
];

export function Settings({ route, me, reloadKey, onRoute }: Props) {
  const { t } = useI18n();
  const isAdmin = me?.role === "admin";

  // A non-admin who follows a link to the admin tab is shown the three tabs they can use
  // rather than an empty table. The API answers 404 regardless; this is presentation.
  const tabs = isAdmin ? TABS : TABS.filter((tab) => tab.id !== "admin");
  const active: SettingsTab =
    !isAdmin && route.settingsTab === "admin" ? "trash" : route.settingsTab;

  useEffect(() => {
    if (!isAdmin && route.settingsTab === "admin") {
      onRoute({ settingsTab: "trash" }, "replace");
    }
  }, [isAdmin, route.settingsTab, onRoute]);

  const heading = tabs.find((tab) => tab.id === active)?.key ?? "settings.trash";

  return (
    <div className="serein-page serein-page--narrow">
      <div className="serein-page-header">
        <div className="serein-page-header__main">
          <p className="serein-page-header__eyebrow">{t("settings.eyebrow")}</p>
          <h1 className="serein-page-header__title">{t("settings.title")}</h1>
          <p className="serein-page-header__description">{t(`settings.subtitle.${active}`)}</p>
        </div>
      </div>

      <div data-serein-tabs>
        <div className="serein-tabs serein-tabs--underline wc-settings-tabs">
          <div className="serein-tabs__list" role="tablist" aria-label={t("settings.tabs")}>
            {tabs.map((tab) => (
              <button
                key={tab.id}
                type="button"
                className="serein-tab"
                role="tab"
                id={`settings-tab-${tab.id}`}
                aria-controls={`settings-panel-${tab.id}`}
                aria-selected={active === tab.id}
                tabIndex={active === tab.id ? 0 : -1}
                onClick={() => onRoute({ settingsTab: tab.id }, "push")}
              >
                <span className="serein-tab__icon">
                  <Icon name={tab.icon} size={12} />
                </span>
                {t(tab.key)}
              </button>
            ))}
          </div>
        </div>

        <div
          className="serein-tabpanel wc-settings-panel"
          role="tabpanel"
          id={`settings-panel-${active}`}
          aria-labelledby={`settings-tab-${active}`}
          aria-label={t(heading)}
          tabIndex={0}
        >
          {active === "trash" ? <Trash reloadKey={reloadKey} /> : null}
          {active === "activity" ? <Activity reloadKey={reloadKey} /> : null}
          {active === "access" ? <Access reloadKey={reloadKey} /> : null}
          {active === "admin" && isAdmin ? <AccountAdmin /> : null}
        </div>
      </div>
    </div>
  );
}

/* ------------------------------------------------------------------ trash --- */

function Trash({ reloadKey }: { reloadKey: number }) {
  const { t, fmt } = useI18n();
  const toast = useToast();
  const [items, setItems] = useState<FileNode[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [emptying, setEmptying] = useState(false);
  const [confirming, setConfirming] = useState(false);

  const load = () => {
    setItems(null);
    setError(null);
    api
      .trash()
      .then((res) => setItems(res.items ?? []))
      .catch((err) => setError(err instanceof APIError ? err.message || err.code : String(err)));
  };

  useEffect(load, [reloadKey]);

  const restore = async (node: FileNode) => {
    setBusy(node.id);
    try {
      await api.restore(node.id);
      toast.success(t("toast.restored"));
      load();
    } catch (err) {
      toast.error(err instanceof APIError ? err.message : String(err));
    } finally {
      setBusy(null);
    }
  };

  /**
   * empty runs the purge, following `more`.
   *
   * The server batches so that a bin with a hundred thousand files cannot time out, and answers
   * `more: true` when it stopped early. Looping here rather than asking the user to press the button
   * again is the difference between "emptied" and "partly emptied, try again" — but the loop is
   * BOUNDED, because an unbounded one against a bin that keeps refilling (a sync client deleting into
   * it) would never return and the button would never come back.
   */
  const empty = async () => {
    setEmptying(true);
    let roots = 0;
    let bytes = 0;
    try {
      for (let round = 0; round < 20; round++) {
        const res = await api.emptyTrash();
        roots += res.purged ?? 0;
        bytes += res.bytes_freed ?? 0;
        if (!res.more) break;
      }
      toast.success(
        bytes > 0
          ? `${t("trash.emptied")} · ${t("trash.purgedBytes", { size: fmt.size(bytes) })}`
          : t("trash.emptied"),
      );
      setConfirming(false);
      load();
    } catch (err) {
      toast.error(err instanceof APIError ? err.message || err.code : String(err));
    } finally {
      setEmptying(false);
      void roots;
    }
  };

  if (error) return <ErrorState message={error} onRetry={load} />;
  if (items === null) return <Skeleton rows={4} />;
  if (items.length === 0) {
    return <EmptyState icon="trash" title={t("trash.empty")} hint={t("trash.emptyHint")} />;
  }

  return (
    <>
      <div className="wc-trash-actions">
        <span className="wc-trash-actions__note">
          {t("trash.emptyHint")}
        </span>
        {/* Two steps, and the second one states the COUNT. "Are you sure?" is not a question a user
            can answer without knowing how much they are about to lose. */}
        {confirming ? (
          <span className="wc-trash-actions__confirm">
            <span className="serein-hint">
              {t("trash.emptyConfirmBody", { n: items.length })}
            </span>
            <button
              type="button"
              className="serein-btn serein-btn--sm serein-btn--danger"
              disabled={emptying}
              onClick={() => void empty()}
            >
              {t("trash.emptyAction")}
            </button>
            <button
              type="button"
              className="serein-btn serein-btn--sm serein-btn--outline"
              disabled={emptying}
              onClick={() => setConfirming(false)}
            >
              {t("social.cancel")}
            </button>
          </span>
        ) : (
          <button
            type="button"
            className="serein-btn serein-btn--sm serein-btn--outline"
            onClick={() => setConfirming(true)}
          >
            <Icon name="trash" size={12} />
            {t("trash.emptyAction")}
          </button>
        )}
      </div>

      <div className="wc-card">
        <div className="wc-list">
          {items.map((node) => (
            <div className="wc-list__row" key={node.id}>
              <FileTile entry={node} size={34} />
              <span className="wc-list__main">
                <span className="wc-list__title">{node.name}</span>
                <span className="wc-list__meta">
                  {kindLabel(t, node)} · {node.kind === "dir" ? "" : `${fmt.size(node.size)} · `}
                  {t("trash.deletedAt", { when: fmt.relative(node.modified_at) })}
                </span>
              </span>
              <button
                type="button"
                className="serein-btn serein-btn--sm serein-btn--outline"
                disabled={busy === node.id}
                onClick={() => void restore(node)}
              >
                <Icon name="refresh" size={12} />
                {t("trash.restore")}
              </button>
            </div>
          ))}
        </div>
      </div>
    </>
  );
}

/* ----------------------------------------------------------------- access --- */

function Access({ reloadKey }: { reloadKey: number }) {
  const { t, fmt } = useI18n();
  const toast = useToast();
  const [items, setItems] = useState<AppPassword[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [creating, setCreating] = useState(false);

  const load = () => {
    setItems(null);
    setError(null);
    api
      .appPasswords()
      .then((res) => setItems(res.items ?? []))
      .catch((err) => setError(err instanceof APIError ? err.message || err.code : String(err)));
  };

  useEffect(load, [reloadKey]);

  const revoke = async (id: string) => {
    try {
      await api.revokeAppPassword(id);
      toast.success(t("toast.revoked"));
      load();
    } catch (err) {
      toast.error(err instanceof APIError ? err.message : String(err));
    }
  };

  // The WebDAV endpoint is this origin, not a configured value: the client cannot know a
  // public hostname it was not told, and inventing one would give people an address that does
  // not resolve. `location.origin` is exactly what the server answered on.
  const davURL = `${location.origin}/dav/`;

  return (
    <div className="wc-stack" style={{ gap: "var(--serein-space-8)" }}>
      <section>
        <div className="serein-section__head" style={{ display: "flex", alignItems: "center", gap: 12 }}>
          <h2 className="wc-section__title">{t("access.appPasswords")}</h2>
          <button
            type="button"
            className="serein-btn serein-btn--outline wc-section__link"
            onClick={() => setCreating(true)}
          >
            <Icon name="plus" size={13} />
            {t("access.newPassword")}
          </button>
        </div>
        <p className="serein-hint wc-settings-note">{t("access.appPasswordHint")}</p>

        {error ? (
          <ErrorState message={error} onRetry={load} />
        ) : items === null ? (
          <Skeleton rows={3} />
        ) : items.length === 0 ? (
          <EmptyState icon="key" title={t("access.empty")} hint={t("access.emptyHint")} />
        ) : (
          <div className="wc-card">
            <div className="wc-list">
              {items.map((pw) => (
                <div className="wc-list__row" key={pw.id}>
                  <span className="serein-inspector__callout-icon" aria-hidden="true">
                    <Icon name="key" size={14} />
                  </span>
                  <span className="wc-list__main">
                    <span className="wc-list__title">{pw.label || t("access.unlabelled")}</span>
                    <span className="wc-list__meta wc-mono">{pw.prefix}…</span>
                  </span>
                  <span className="serein-badge serein-badge--outline">
                    {pw.scope === "ro" ? t("access.scopeRO") : t("access.scopeRW")}
                  </span>
                  <span className="wc-list__end wc-list__end--secondary">
                    {pw.last_used ? t("access.usedAt", { when: fmt.relative(pw.last_used) }) : t("access.never")}
                  </span>
                  {pw.revoked ? (
                    <span className="serein-badge serein-badge--warning">{t("access.revoked")}</span>
                  ) : (
                    <button
                      type="button"
                      className="serein-btn serein-btn--sm serein-btn--quiet"
                      onClick={() => void revoke(pw.id)}
                    >
                      {t("shared.revoke")}
                    </button>
                  )}
                </div>
              ))}
            </div>
          </div>
        )}
      </section>

      <section>
        <h2 className="wc-section__title">{t("access.webdav")}</h2>
        <p className="serein-hint wc-settings-note">{t("access.webdavHint")}</p>
        <div className="serein-link-box">
          <span className="serein-link-box__url">{davURL}</span>
          <button
            type="button"
            className="serein-btn serein-btn--sm serein-btn--outline"
            onClick={() => {
              void navigator.clipboard?.writeText(davURL).then(
                () => toast.success(t("inspector.copied")),
                () => toast.error(t("inspector.copyFailed")),
              );
            }}
          >
            {t("inspector.copy")}
          </button>
        </div>
      </section>

      <FolderAccess />

      {creating ? (
        <NewPasswordDialog onClose={() => setCreating(false)} onDone={load} />
      ) : null}
    </div>
  );
}
