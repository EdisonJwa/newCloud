// Administration: accounts, groups, and the runtime settings.
//
// Why this exists at all: several deployment policies (including the default `pending` JIT policy)
// create accounts that cannot sign in until an administrator approves them, and the API for that was
// built and tested while nothing in the browser could reach it. The policy therefore had a state it
// could put a person into with no way out except curl. The same shape applied twice more: groups and
// runtime settings were fully implemented endpoints with no screen, which made group sharing
// unreachable and an override invisible.
//
// Three panels rather than one long page, because they answer different questions at different
// times: "who is waiting", "who shares what with whom", and "what are the limits right now".
//
// The view is gated on the caller's own role. Gating is presentation only: the API answers 404 to a
// non-admin, so a user who forces this section to render gets an empty table, not a privilege.

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
    <>
      <div className="viewhead">
        <h1>{t("admin.title")}</h1>
      </div>

      <div className="adminfilters" role="group" aria-label={t("admin.tabs")}>
        {(["accounts", "groups", "settings"] as const).map((p) => (
          <button
            key={p}
            type="button"
            className={panel === p ? "btn primary" : "btn"}
            aria-pressed={panel === p}
            onClick={() => setPanel(p)}
          >
            {p === "accounts" ? t("admin.tabAccounts") : p === "groups" ? t("admin.tabGroups") : t("admin.tabSettings")}
          </button>
        ))}
      </div>

      {panel === "accounts" ? <Accounts /> : null}
      {panel === "groups" ? <Groups /> : null}
      {panel === "settings" ? <Settings /> : null}
    </>
  );
}

/* ---------- accounts ---------- */

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
      toast.info(t("admin.statusChanged", { name: u.username, status: t(`admin.status.${status}`) }));
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
      toast.info(t("admin.quotaSaved", { name: u.username }));
      setEditing(null);
      load(filter);
    } catch (err) {
      toast.error(errText(err));
    } finally {
      setBusy(null);
    }
  };

  return (
    <>
      <p className="hintline">{t("admin.hint")}</p>

      <div className="adminfilters" role="group" aria-label={t("admin.filter")}>
        {(["pending", "active", "disabled", "all"] as const).map((f) => (
          <button
            key={f}
            type="button"
            className={filter === f ? "btn primary" : "btn"}
            aria-pressed={filter === f}
            onClick={() => setFilter(f)}
          >
            {f === "all" ? t("admin.filterAll") : t(`admin.status.${f}`)}
          </button>
        ))}
        <button type="button" className="btn" onClick={() => load(filter)}>
          <Icon name="refresh" size={13} />
          {t("admin.reload")}
        </button>
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
        <div className="tablewrap">
          <table className="table">
            <thead>
              <tr>
                <th>{t("admin.account")}</th>
                <th>{t("admin.role")}</th>
                <th>{t("admin.status")}</th>
                <th className="end">{t("admin.quota")}</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {users.map((u) => (
                <tr key={u.id}>
                  <td>
                    {u.display_name || u.username}
                    {u.is_breakglass ? <span className="tag">{t("admin.breakglass")}</span> : null}
                    <br />
                    <span className="sub">{u.email}</span>
                  </td>
                  <td>{u.role}</td>
                  <td>
                    <span className={`status status-${u.status}`}>
                      {t(`admin.status.${u.status}`)}
                    </span>
                  </td>
                  <td className="end mono">
                    {editing === u.id ? (
                      <span className="quota-edit">
                        <input
                          type="number"
                          min="0"
                          value={quotaDraft}
                          aria-label={t("admin.quotaBytes")}
                          onChange={(e) => setQuotaDraft(e.target.value)}
                        />
                        <button
                          type="button"
                          className="btn sm primary"
                          disabled={busy === u.id}
                          onClick={() => void saveQuota(u)}
                        >
                          {t("admin.save")}
                        </button>
                        <button type="button" className="btn sm ghost" onClick={() => setEditing(null)}>
                          {t("dlg.cancel")}
                        </button>
                      </span>
                    ) : (
                      <>
                        {fmt.size(u.used_bytes ?? 0)} / {u.quota_bytes ? fmt.size(u.quota_bytes) : "∞"}
                        <button
                          type="button"
                          className="btn sm ghost"
                          onClick={() => {
                            setEditing(u.id);
                            setQuotaDraft(String(u.quota_bytes ?? 0));
                          }}
                        >
                          {t("admin.setQuota")}
                        </button>
                      </>
                    )}
                  </td>
                  <td className="end">
                    {TARGETS[u.status].map((target) => (
                      <button
                        key={target}
                        type="button"
                        className="btn ghost"
                        disabled={busy === u.id}
                        onClick={() => setStatus(u, target)}
                      >
                        {t(`admin.action.${target}`)}
                      </button>
                    ))}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

/* ---------- groups ---------- */

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
      toast.info(t("admin.groupCreated", { name: name.trim() }));
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
      toast.info(t("admin.memberAdded", { name: username }));
      setAddTo((prev) => ({ ...prev, [group.id]: "" }));
      load();
    } catch (err) {
      toast.error(errText(err));
    }
  };

  const removeMember = async (group: Group, userID: string, username: string) => {
    try {
      await api.removeGroupMember(group.id, userID);
      toast.info(t("admin.memberRemoved", { name: username }));
      load();
    } catch (err) {
      toast.error(errText(err));
    }
  };

  return (
    <>
      <p className="hintline">{t("admin.groupsHint")}</p>

      <div className="aclbar">
        <input
          value={name}
          placeholder={t("admin.groupName")}
          aria-label={t("admin.groupName")}
          onChange={(e) => setName(e.target.value)}
        />
        <input
          value={display}
          placeholder={t("admin.groupDisplay")}
          aria-label={t("admin.groupDisplay")}
          onChange={(e) => setDisplay(e.target.value)}
        />
        <button type="button" className="btn primary" disabled={busy || !name.trim()} onClick={create}>
          <Icon name="folderPlus" size={13} />
          {t("admin.createGroup")}
        </button>
      </div>

      {error ? (
        <ErrorState message={error} onRetry={load} />
      ) : groups === null ? (
        <Skeleton rows={3} />
      ) : groups.length === 0 ? (
        <EmptyState icon="shares" title={t("admin.groupsEmpty")} hint={t("admin.groupsHint")} />
      ) : (
        <div className="groups">
          {groups.map((group) => (
            <section className="block groupcard" key={group.id}>
              <h2>
                <span>{group.display_name || group.name}</span>
                <span className="spacer" />
                <span className="mono sub">{group.name}</span>
              </h2>

              <div className="chips">
                {(group.members ?? []).length === 0 ? (
                  <span className="hintline">{t("admin.noMembers")}</span>
                ) : (
                  (group.members ?? []).map((m) => (
                    <span className="chip" key={m.id}>
                      {m.username}
                      <button
                        type="button"
                        aria-label={t("admin.memberRemoved", { name: m.username })}
                        onClick={() => void removeMember(group, m.id, m.username)}
                      >
                        <Icon name="close" size={9} />
                      </button>
                    </span>
                  ))
                )}
              </div>

              <div className="aclbar">
                <input
                  value={addTo[group.id] ?? ""}
                  placeholder={t("admin.member")}
                  aria-label={t("admin.member")}
                  onChange={(e) => setAddTo((prev) => ({ ...prev, [group.id]: e.target.value }))}
                  onKeyDown={(e) => {
                    if (e.key === "Enter") void addMember(group);
                  }}
                />
                <button
                  type="button"
                  className="btn sm"
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
    </>
  );
}

/* ---------- runtime settings ---------- */

function Settings() {
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
      toast.info(t("admin.settingSaved", { key, value }));
      setEditing(null);
      load();
    } catch (err) {
      toast.error(errText(err));
    }
  };

  const reset = async (key: string) => {
    try {
      await api.resetAdminSetting(key);
      toast.info(t("admin.settingReset", { key }));
      load();
    } catch (err) {
      toast.error(errText(err));
    }
  };

  return (
    <>
      <p className="hintline">{t("admin.settingsHint")}</p>

      {error ? (
        <ErrorState message={error} onRetry={load} />
      ) : items === null ? (
        <Skeleton rows={5} />
      ) : items.length === 0 ? (
        <EmptyState icon="grid" title={t("admin.empty")} />
      ) : (
        <div className="tablewrap">
          <table className="table">
            <thead>
              <tr>
                <th>{t("admin.settingKey")}</th>
                <th className="end">{t("admin.settingValue")}</th>
                <th>{t("admin.settingSource")}</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {items.map((s) => (
                <tr key={s.key}>
                  <td className="mono">{s.key}</td>
                  <td className="end mono">
                    {editing === s.key ? (
                      <input
                        type="number"
                        value={draft}
                        aria-label={t("admin.settingValue")}
                        onChange={(e) => setDraft(e.target.value)}
                      />
                    ) : (
                      s.value
                    )}
                  </td>
                  <td>
                    {/* The source is shown, not inferred: a value that happens to equal the default
                        is a different fact from one that is pinned by an override. */}
                    <span className={`pill${s.source === "override" ? " accent" : ""}`}>
                      {s.source === "override" ? t("admin.sourceOverride") : t("admin.sourceConfig")}
                    </span>
                  </td>
                  <td className="end">
                    {editing === s.key ? (
                      <>
                        <button type="button" className="btn sm primary" onClick={() => void save(s.key)}>
                          {t("admin.save")}
                        </button>
                        <button type="button" className="btn sm ghost" onClick={() => setEditing(null)}>
                          {t("dlg.cancel")}
                        </button>
                      </>
                    ) : (
                      <>
                        <button
                          type="button"
                          className="btn sm ghost"
                          onClick={() => {
                            setEditing(s.key);
                            setDraft(String(s.value));
                          }}
                        >
                          {t("admin.edit")}
                        </button>
                        {s.source === "override" ? (
                          <button type="button" className="btn sm ghost" onClick={() => void reset(s.key)}>
                            {t("admin.reset")}
                          </button>
                        ) : null}
                      </>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}
