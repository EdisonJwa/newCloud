// Account administration: the approval queue and the account list.
//
// Why this exists at all: severaldeployment policies (including the default `pending` JIT
// policy) create accounts that cannot sign in until an administrator approves them. The API for
// that — `/api/admin/users?status=pending` and `POST /api/admin/users/{id}/status` — was built
// and tested, but nothing in the browser could reach it. The policy therefore had a state it
// could put a person into with no way out except curl. That is the shape of gap worth closing:
// not a missing feature but an unreachable one.
//
// The view is gated on the caller's own role. Gating is presentation only: the API returns 404 to
// a non-admin, so a user who forces this section to render gets an empty table, not a privilege.

import { useEffect, useState } from "react";
import { api, APIError } from "../api";
import { useI18n } from "../i18n";
import { EmptyState, ErrorState, Icon, Skeleton, useToast } from "../ui";
import type { AdminUser } from "../types";

type Status = "active" | "pending" | "disabled";

/** The statuses an administrator can move an account to, in the order they are offered.
 *  `disabled` is reachable from `active`; `active` is reachable from both others. */
const TARGETS: Record<Status, Status[]> = {
  pending: ["active", "disabled"],
  disabled: ["active"],
  active: ["disabled"],
};

export function AccountAdmin() {
  const { t, fmt } = useI18n();
  const toast = useToast();

  const [filter, setFilter] = useState<Status | "all">("pending");
  const [users, setUsers] = useState<AdminUser[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState<string | null>(null);

  const load = (target: Status | "all") => {
    setUsers(null);
    setError(null);
    api
      .adminUsers(target === "all" ? {} : { status: target })
      .then((res) => setUsers(res?.users ?? []))
      .catch((err) => setError(err instanceof APIError ? err.message || err.code : String(err)));
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
      toast.error(err instanceof APIError ? err.message || err.code : String(err));
    } finally {
      setBusy(null);
    }
  };

  return (
    <>
      <div className="viewhead">
        <h1>{t("admin.title")}</h1>
        <span className="spacer" />
        <button type="button" className="btn" onClick={() => load(filter)}>
          <Icon name="refresh" size={13} />
          {t("admin.reload")}
        </button>
      </div>

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
                    {fmt.size(u.used_bytes ?? 0)} / {u.quota_bytes ? fmt.size(u.quota_bytes) : "∞"}
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
