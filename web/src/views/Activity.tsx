// Activity — the audit log.
//
// This is the product's distinguishing claim made readable. Two decisions matter:
//
//  * The default is chronological, grouped by day, with denied attempts marked. An audit log
//    whose refusals are hidden is not an audit log; the interesting rows are the ones that did
//    not succeed.
//  * Export is a plain link to the server's CSV, not a client-side serialisation. The client
//    must never be the thing that decides what the authoritative record contained.
//
// The timeline is a rail with dots rather than a table because an audit record is read in
// order: what happened, then what happened next. A table invites scanning by column, which is
// the wrong question for a log.

import { useCallback, useEffect, useState } from "react";
import { api, APIError } from "../api";
import { useI18n } from "../i18n";
import { EmptyState, ErrorState, Icon, Skeleton } from "../ui";
import type { AuditEvent, AuditPage } from "../types";

export function Activity({ reloadKey }: { reloadKey: number }) {
  const { t, fmt } = useI18n();
  const [page, setPage] = useState<AuditPage | null>(null);
  const [events, setEvents] = useState<AuditEvent[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [action, setAction] = useState("");
  const [result, setResult] = useState("");
  const [since, setSince] = useState("");
  const [loadingMore, setLoadingMore] = useState(false);

  const load = useCallback(
    async (offset: number, replace: boolean) => {
      setError(null);
      try {
        const res = await api.audit({
          offset,
          ...(action ? { actions: action } : {}),
          ...(result ? { result } : {}),
          ...(since ? { since } : {}),
        });
        setPage(res);
        setEvents((prev) => (replace ? (res.events ?? []) : [...prev, ...(res.events ?? [])]));
      } catch (err) {
        setError(err instanceof APIError ? err.message || err.code : String(err));
      }
    },
    [action, result, since],
  );

  useEffect(() => {
    setPage(null);
    void load(0, true);
  }, [load, reloadKey]);

  const groups = groupByDay(events, fmt.day);
  const filtered = Boolean(action || result || since);

  return (
    <div className="wc-stack" style={{ gap: "var(--serein-space-6)" }}>
      <div className="wc-activity-filters">
        <label className="serein-field wc-activity-filter">
          <span className="serein-label">{t("activity.action")}</span>
          <span className="serein-select-wrap">
            <select
              className="serein-select serein-select--sm"
              value={action}
              onChange={(e) => setAction(e.target.value)}
            >
              <option value="">{t("activity.allActions")}</option>
              {(page?.actions ?? []).map((a) => (
                <option key={a} value={a}>
                  {a}
                </option>
              ))}
            </select>
          </span>
        </label>

        <label className="serein-field wc-activity-filter">
          <span className="serein-label">{t("activity.result")}</span>
          <span className="serein-select-wrap">
            <select
              className="serein-select serein-select--sm"
              value={result}
              onChange={(e) => setResult(e.target.value)}
            >
              <option value="">{t("activity.anyResult")}</option>
              <option value="ok">{t("activity.allowed")}</option>
              <option value="denied">{t("activity.refused")}</option>
            </select>
          </span>
        </label>

        <label className="serein-field wc-activity-filter">
          <span className="serein-label">{t("activity.since")}</span>
          <span className="serein-select-wrap">
            <select
              className="serein-select serein-select--sm"
              value={since}
              onChange={(e) => setSince(e.target.value)}
            >
              <option value="">{t("activity.allTime")}</option>
              <option value="1h">{t("activity.lastHour")}</option>
              <option value="24h">{t("activity.last24")}</option>
              <option value="7d">{t("activity.last7")}</option>
              <option value="30d">{t("activity.last30")}</option>
            </select>
          </span>
        </label>

        <span className="wc-grow" />

        {page ? (
          <span className="serein-hint wc-numeric">{t("count.events", { n: page.total })}</span>
        ) : null}

        <a
          className="serein-btn serein-btn--sm serein-btn--outline"
          href={`/api/audit?format=csv${since ? `&since=${since}` : ""}`}
          download
        >
          <Icon name="download" size={12} />
          {t("activity.export")}
        </a>
      </div>

      {error ? (
        <ErrorState message={error} onRetry={() => void load(0, true)} />
      ) : page === null ? (
        <Skeleton rows={6} />
      ) : events.length === 0 ? (
        <EmptyState
          icon="pulse"
          title={filtered ? t("activity.filtered") : t("activity.empty")}
          hint={filtered ? t("activity.filteredHint") : t("activity.emptyHint")}
        />
      ) : (
        <div className="wc-timeline">
          {groups.map(([day, rows]) => (
            <div key={day} className="wc-timeline__group">
              <h3 className="wc-timeline__day">{day}</h3>
              {rows.map((event) => (
                <div
                  key={event.id}
                  className={`wc-timeline__row${event.result === "denied" ? " wc-timeline__row--denied" : ""}`}
                >
                  <span className="wc-timeline__when">
                    <span className="wc-mono">{fmt.clock(event.ts)}</span>
                    <span>{fmt.relative(event.ts)}</span>
                  </span>
                  <span className="wc-timeline__rail" aria-hidden="true">
                    <span
                      className={`serein-status-dot${event.result === "denied" ? " serein-status-dot--danger" : ""}`}
                    />
                  </span>
                  <span className="wc-timeline__body">
                    <span className="wc-timeline__head">
                      <span className="wc-timeline__action">{event.action}</span>
                      <span className="wc-timeline__target">{targetOf(event)}</span>
                    </span>
                    <span className="wc-timeline__meta">
                      <span>{whoOf(event, t)}</span>
                      {event.result !== "ok" ? (
                        <span className="serein-badge serein-badge--danger">{event.result}</span>
                      ) : null}
                      {event.bytes ? <span>{fmt.size(event.bytes)}</span> : null}
                      {event.actor_ip ? <span className="wc-mono">{event.actor_ip}</span> : null}
                      {event.share_id ? <span>{t("activity.viaLink")}</span> : null}
                    </span>
                  </span>
                </div>
              ))}
            </div>
          ))}

          <div className="wc-timeline__more">
            {page?.has_more ? (
              <button
                type="button"
                className="serein-btn serein-btn--sm serein-btn--outline"
                disabled={loadingMore}
                onClick={async () => {
                  setLoadingMore(true);
                  await load(events.length, false);
                  setLoadingMore(false);
                }}
              >
                {loadingMore ? <span className="serein-spinner" aria-hidden="true" /> : null}
                {t("activity.loadOlder")}
              </button>
            ) : (
              <span className="serein-hint">{t("activity.end")}</span>
            )}
          </div>
        </div>
      )}
    </div>
  );
}

function groupByDay(events: AuditEvent[], label: (iso: string) => string): Array<[string, AuditEvent[]]> {
  const out: Array<[string, AuditEvent[]]> = [];
  for (const event of events) {
    const day = label(event.ts);
    const last = out[out.length - 1];
    if (last && last[0] === day) last[1].push(event);
    else out.push([day, [event]]);
  }
  return out;
}

function targetOf(event: AuditEvent): string {
  const meta = event.metadata ?? {};
  const name = typeof meta.name === "string" ? meta.name : "";
  const path = typeof meta.path === "string" ? meta.path : "";
  if (path) return path;
  if (name) return name;
  if (event.target_id) return event.target_id.slice(0, 8);
  return "—";
}

function whoOf(event: AuditEvent, t: (key: string) => string): string {
  if (event.actor_email) return event.actor_email;
  if (event.actor_kind === "user") return event.actor_user_id?.slice(0, 8) ?? t("activity.system");
  if (event.actor_kind === "anonymous") return t("activity.anonymous");
  return event.actor_kind || t("activity.system");
}
