// Administration: accounts, groups, and the runtime settings.
//
// Why this exists at all: several deployment policies (including the default `pending` JIT
// policy) create accounts that cannot sign in until an administrator approves them, and the API
// for that was built and tested while nothing in the browser could reach it. The policy
// therefore had a state it could put a person into with no way out except curl. The same shape
// applied twice more: groups and runtime settings were fully implemented endpoints with no
// screen, which made group sharing unreachable and an override invisible.
//
// Three panels rather than one long page, because they answer different questions at different
// times: "who is waiting", "who shares what with whom", and "what are the limits right now".
//
// The view is gated on the caller's own role. Gating is presentation only: the API answers 404
// to a non-admin, so a user who forces this section to render gets an empty table, not a
// privilege.

import { useEffect, useState } from "react";
import { api, APIError } from "../api";
import { useI18n } from "../i18n";
import { EmptyState, ErrorState, Icon, Skeleton, useToast } from "../ui";
import type { AdminSetting, AdminUser, Group } from "../types";

type Status = "active" | "pending" | "disabled";
type Panel = "accounts" | "groups" | "settings";

/** The statuses an administrator can move an account to, in the order they are offered.
 *  `disabled` is reachable from `active`; `active` is reachable from both others. */
const TARGETS: Record<Status, Status[]> = {
  pending: ["active", "disabled"],
  disabled: ["active"],
  active: ["disabled"],
};

function errText(err: unknown): string {
  return err instanceof APIError ? err.message || err.code : String(err);
}

export function AccountAdmin() {
  const { t } = useI18n();
  const [panel, setPanel] = useState<Panel>("accounts");

  return (
    <div className="wc-stack" style={{ gap: "var(--serein-space-6)" }}>
      <div className="serein-tabs serein-tabs--segmented">
        <div className="serein-tabs__list" role="tablist" aria-label={t("admin.tabs")}>
          {(["accounts", "groups", "settings"] as const).map((p) => (
            <button
              key={p}
              type="button"
              className="serein-tab"
              role="tab"
              id={`admin-tab-${p}`}
              aria-controls={`admin-panel-${p}`}
              aria-selected={panel === p}
              tabIndex={panel === p ? 0 : -1}
              onClick={() => setPanel(p)}
            >
              {p === "accounts" ? t("admin.tabAccounts") : p === "groups" ? t("admin.tabGroups") : t("admin.tabSettings")}
            </button>
          ))}
        </div>
      </div>

      <div role="tabpanel" id={`admin-panel-${panel}`} aria-labelledby={`admin-tab-${panel}`} tabIndex={0}>
        {panel === "accounts" ? <Accounts /> : null}
        {panel === "groups" ? <Groups /> : null}
        {panel === "settings" ? <RuntimeSettings /> : null}
      </div>
    </div>
  );
}

/* ---------------------------------------------------------------- accounts --- */

function Accounts() {
  const { t, fmt } = useI18n();
  const toast = useToast();

  const [filter, setFilter] = useState<Status | "all">("pending");
  const [users, setUsers] = useState<AdminUser[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [editing, setEditing] = useState<string | null>(null);
  const [quotaDraft, setQuotaDraft] = useState("");

  const load = (target: Status | "all") => {
    setUsers(null);
    setError(null);
    api
      .adminUsers(target === "all" ? {} : { status: target })
      .then((res) => setUsers(res?.users ?? []))
      .catch((err) => setError(errText(err)));
  };

  useEffect(() => {
    load(filter);
  }, [filter]);

  const setStatus = async (u: AdminUser, status: Status) => {
    setBusy(u.id);
    try {
      await api.setUserStatus(u.id, status);
      toast.success(t("admin.statusChanged", { name: u.username, status: t(`admin.status.${status}`) }));
      load(filter);
    } catch (err) {
      toast.error(errText(err));
    } finally {
      setBusy(null);
    }
  };

  const saveQuota = async (u: AdminUser) => {
    const bytes = Number.parseInt(quotaDraft, 10);
    if (Number.isNaN(bytes) || bytes < 0) {
      toast.error(t("admin.quotaInvalid"));
      return;
    }
    setBusy(u.id);
    try {
      await api.setUserQuota(u.id, bytes);
      toast.success(t("admin.quotaSaved", { name: u.username }));
      setEditing(null);
      load(filter);
    } catch (err) {
      toast.error(errText(err));
    } finally {
      setBusy(null);
    }
  };

  return (
    <div className="wc-stack" style={{ gap: "var(--serein-space-5)" }}>
      <p className="serein-hint">{t("admin.hint")}</p>

      <div className="serein-actionbar">
        <div className="serein-actionbar__filters" role="group" aria-label={t("admin.filter")}>
          {(["pending", "active", "disabled", "all"] as const).map((f) => (
            <button
              key={f}
              type="button"
              className="serein-filter"
              aria-pressed={filter === f}
              onClick={() => setFilter(f)}
            >
              {f === "all" ? t("admin.filterAll") : t(`admin.status.${f}`)}
            </button>
          ))}
        </div>
        <div className="serein-actionbar__tools">
          <button type="button" className="serein-btn serein-btn--sm serein-btn--outline" onClick={() => load(filter)}>
            <Icon name="refresh" size={12} />
            {t("admin.reload")}
          </button>
        </div>
      </div>

      {error ? (
        <ErrorState message={error} onRetry={() => load(filter)} />
      ) : users === null ? (
        <Skeleton rows={4} />
      ) : users.length === 0 ? (
        <EmptyState
          icon="user"
          title={filter === "pending" ? t("admin.emptyPending") : t("admin.empty")}
          hint={filter === "pending" ? t("admin.emptyPendingHint") : t("admin.emptyHint")}
        />
      ) : (
        <div className="wc-card">
          <div className="wc-list">
            {users.map((u) => (
              <div className="wc-list__row" key={u.id}>
                <span className="serein-avatar" aria-hidden="true">
                  {(u.display_name || u.username).slice(0, 2).toUpperCase()}
                </span>
                <span className="wc-list__main">
                  <span className="wc-list__title">
                    {u.display_name || u.username}
                    {u.is_breakglass ? (
                      <span className="serein-badge serein-badge--warning">{t("admin.breakglass")}</span>
                    ) : null}
                  </span>
                  <span className="wc-list__meta">
                    {u.email} · {t("admin.roleNamed", { role: u.role })}
                  </span>
                </span>

                <span
                  className={`serein-badge serein-badge--${u.status === "active" ? "success" : u.status === "pending" ? "warning" : "outline"}`}
                >
                  {t(`admin.status.${u.status}`)}
                </span>

                <span className="wc-list__end wc-list__end--secondary wc-numeric">
                  {editing === u.id ? (
                    <span className="wc-quota-edit">
                      <span className="serein-input-wrap serein-input-wrap--sm">
                        <input
                          className="serein-input"
                          type="number"
                          min="0"
                          value={quotaDraft}
                          aria-label={t("admin.quotaBytes")}
                          onChange={(e) => setQuotaDraft(e.target.value)}
                          onKeyDown={(e) => {
                            if (e.key === "Enter") void saveQuota(u);
                            if (e.key === "Escape") setEditing(null);
                          }}
                        />
                      </span>
                      <button
                        type="button"
                        className="serein-btn serein-btn--sm"
                        disabled={busy === u.id}
                        onClick={() => void saveQuota(u)}
                      >
                        {t("admin.save")}
                      </button>
                    </span>
                  ) : (
                    <button
                      type="button"
                      className="serein-breadcrumb__link"
                      title={t("admin.setQuota")}
                      onClick={() => {
                        setEditing(u.id);
                        setQuotaDraft(String(u.quota_bytes ?? 0));
                      }}
                    >
                      {fmt.size(u.used_bytes ?? 0)}
                      {u.quota_bytes ? ` / ${fmt.size(u.quota_bytes)}` : ` / ${t("home.unlimited")}`}
                    </button>
                  )}
                </span>

                <span className="wc-admin-actions">
                  {TARGETS[u.status as Status]?.map((target) => (
                    <button
                      key={target}
                      type="button"
                      className={`serein-btn serein-btn--sm ${target === "disabled" ? "serein-btn--quiet" : "serein-btn--outline"}`}
                      disabled={busy === u.id}
                      onClick={() => void setStatus(u, target)}
                    >
                      {t(`admin.action.${target}`)}
                    </button>
                  ))}
                </span>
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}

/* ------------------------------------------------------------------ groups --- */

function Groups() {
  const { t } = useI18n();
  const toast = useToast();

  const [groups, setGroups] = useState<Group[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [name, setName] = useState("");
  const [display, setDisplay] = useState("");
  const [busy, setBusy] = useState(false);
  // addTo maps a group id to the username being typed for it, so two open forms do not fight.
  const [addTo, setAddTo] = useState<Record<string, string>>({});

  const load = () => {
    setGroups(null);
    setError(null);
    api
      .adminGroups()
      .then((res) => setGroups(res.groups ?? []))
      .catch((err) => setError(errText(err)));
  };

  useEffect(load, []);

  const create = async () => {
    if (!name.trim()) return;
    setBusy(true);
    try {
      await api.createGroup({ name: name.trim(), display_name: display.trim() || undefined });
      toast.success(t("admin.groupCreated", { name: name.trim() }));
      setName("");
      setDisplay("");
      load();
    } catch (err) {
      toast.error(errText(err));
    } finally {
      setBusy(false);
    }
  };

  const addMember = async (group: Group) => {
    const username = (addTo[group.id] ?? "").trim();
    if (!username) return;
    try {
      await api.addGroupMember(group.id, { username, role: "viewer" });
      toast.success(t("admin.memberAdded", { name: username }));
      setAddTo((prev) => ({ ...prev, [group.id]: "" }));
      load();
    } catch (err) {
      toast.error(errText(err));
    }
  };

  const removeMember = async (group: Group, userID: string, username: string) => {
    try {
      await api.removeGroupMember(group.id, userID);
      toast.success(t("admin.memberRemoved", { name: username }));
      load();
    } catch (err) {
      toast.error(errText(err));
    }
  };

  return (
    <div className="wc-stack" style={{ gap: "var(--serein-space-5)" }}>
      <p className="serein-hint">{t("admin.groupsHint")}</p>

      <form
        className="wc-inlineform"
        onSubmit={(e) => {
          e.preventDefault();
          void create();
        }}
      >
        <div className="serein-field">
          <label className="serein-label" htmlFor="group-name">
            {t("admin.groupName")}
          </label>
          <div className="serein-input-wrap serein-input-wrap--sm">
            <input
              id="group-name"
              className="serein-input"
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </div>
        </div>
        <div className="serein-field">
          <label className="serein-label" htmlFor="group-display">
            {t("admin.groupDisplay")}
          </label>
          <div className="serein-input-wrap serein-input-wrap--sm">
            <input
              id="group-display"
              className="serein-input"
              value={display}
              onChange={(e) => setDisplay(e.target.value)}
            />
          </div>
        </div>
        <button type="submit" className="serein-btn" disabled={busy || !name.trim()}>
          <Icon name="folderPlus" size={13} />
          {t("admin.createGroup")}
        </button>
      </form>

      {error ? (
        <ErrorState message={error} onRetry={load} />
      ) : groups === null ? (
        <Skeleton rows={3} />
      ) : groups.length === 0 ? (
        <EmptyState icon="shares" title={t("admin.groupsEmpty")} hint={t("admin.groupsHint")} />
      ) : (
        <div className="wc-stack" style={{ gap: "var(--serein-space-5)" }}>
          {groups.map((group) => (
            <section className="wc-card wc-card__padding" key={group.id}>
              <div className="wc-row" style={{ marginBottom: 10 }}>
                <h3 className="wc-section__title">{group.display_name || group.name}</h3>
                <span className="serein-hint wc-mono">{group.name}</span>
                <span className="wc-grow" />
                <span className="serein-hint wc-numeric">
                  {t("count.members", { n: (group.members ?? []).length })}
                </span>
              </div>

              <div className="wc-chips">
                {(group.members ?? []).length === 0 ? (
                  <span className="serein-hint">{t("admin.noMembers")}</span>
                ) : (
                  (group.members ?? []).map((m) => (
                    <span className="serein-chip" key={m.id}>
                      {m.username}
                      <button
                        type="button"
                        className="serein-chip__remove"
                        aria-label={t("admin.memberRemoved", { name: m.username })}
                        onClick={() => void removeMember(group, m.id, m.username)}
                      >
                        <Icon name="close" size={10} />
                      </button>
                    </span>
                  ))
                )}
              </div>

              <div className="wc-row" style={{ marginTop: 12 }}>
                <span className="serein-input-wrap serein-input-wrap--sm wc-grow">
                  <input
                    className="serein-input"
                    value={addTo[group.id] ?? ""}
                    placeholder={t("admin.member")}
                    aria-label={t("admin.member")}
                    onChange={(e) => setAddTo((prev) => ({ ...prev, [group.id]: e.target.value }))}
                    onKeyDown={(e) => {
                      if (e.key === "Enter") void addMember(group);
                    }}
                  />
                </span>
                <button
                  type="button"
                  className="serein-btn serein-btn--sm serein-btn--outline"
                  disabled={!(addTo[group.id] ?? "").trim()}
                  onClick={() => void addMember(group)}
                >
                  {t("admin.addMember")}
                </button>
              </div>
            </section>
          ))}
        </div>
      )}
    </div>
  );
}

/* -------------------------------------------------------- runtime settings --- */

function RuntimeSettings() {
  const { t } = useI18n();
  const toast = useToast();

  const [items, setItems] = useState<AdminSetting[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [editing, setEditing] = useState<string | null>(null);
  const [draft, setDraft] = useState("");

  const load = () => {
    setItems(null);
    setError(null);
    api
      .adminSettings()
      .then((res) => setItems(res.settings ?? []))
      .catch((err) => setError(errText(err)));
  };

  useEffect(load, []);

  const save = async (key: string) => {
    const value = Number.parseInt(draft, 10);
    if (Number.isNaN(value)) {
      toast.error(t("admin.settingInvalid"));
      return;
    }
    try {
      await api.setAdminSetting(key, value);
      toast.success(t("admin.settingSaved", { key, value }));
      setEditing(null);
      load();
    } catch (err) {
      toast.error(errText(err));
    }
  };

  const reset = async (key: string) => {
    try {
      await api.resetAdminSetting(key);
      toast.success(t("admin.settingReset", { key }));
      load();
    } catch (err) {
      toast.error(errText(err));
    }
  };

  return (
    <div className="wc-stack" style={{ gap: "var(--serein-space-5)" }}>
      <p className="serein-hint">{t("admin.settingsHint")}</p>

      {error ? (
        <ErrorState message={error} onRetry={load} />
      ) : items === null ? (
        <Skeleton rows={5} />
      ) : items.length === 0 ? (
        <EmptyState icon="grid" title={t("admin.empty")} />
      ) : (
        <div className="wc-card">
          <div className="wc-list">
            {items.map((s) => (
              <div className="wc-list__row" key={s.key}>
                <span className="wc-list__main">
                  <span className="wc-list__title wc-mono">{s.key}</span>
                  <span className="wc-list__meta">{t("admin.settingSourceHint")}</span>
                </span>

                {/* The source is shown, not inferred: a value that happens to equal the default
                    is a different fact from one that is pinned by an override. */}
                <span
                  className={`serein-badge ${s.source === "override" ? "serein-badge--accent" : "serein-badge--outline"}`}
                >
                  {s.source === "override" ? t("admin.sourceOverride") : t("admin.sourceConfig")}
                </span>

                <span className="wc-list__end wc-numeric">
                  {editing === s.key ? (
                    <span className="wc-quota-edit">
                      <span className="serein-input-wrap serein-input-wrap--sm">
                        <input
                          className="serein-input"
                          type="number"
                          value={draft}
                          aria-label={t("admin.settingValue")}
                          onChange={(e) => setDraft(e.target.value)}
                          onKeyDown={(e) => {
                            if (e.key === "Enter") void save(s.key);
                            if (e.key === "Escape") setEditing(null);
                          }}
                        />
                      </span>
                      <button type="button" className="serein-btn serein-btn--sm" onClick={() => void save(s.key)}>
                        {t("admin.save")}
                      </button>
                      <button
                        type="button"
                        className="serein-btn serein-btn--sm serein-btn--quiet"
                        onClick={() => setEditing(null)}
                      >
                        {t("dlg.cancel")}
                      </button>
                    </span>
                  ) : (
                    <span className="wc-mono">{s.value}</span>
                  )}
                </span>

                <span className="wc-admin-actions">
                  {editing === s.key ? null : (
                    <>
                      <button
                        type="button"
                        className="serein-btn serein-btn--sm serein-btn--quiet"
                        onClick={() => {
                          setEditing(s.key);
                          setDraft(String(s.value));
                        }}
                      >
                        {t("admin.edit")}
                      </button>
                      {s.source === "override" ? (
                        <button
                          type="button"
                          className="serein-btn serein-btn--sm serein-btn--quiet"
                          onClick={() => void reset(s.key)}
                        >
                          {t("admin.reset")}
                        </button>
                      ) : null}
                    </>
                  )}
                </span>
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}
