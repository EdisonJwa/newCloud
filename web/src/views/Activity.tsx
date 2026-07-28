// Activity — the audit log.
//
// This is the product's distinguishing claim made readable. Two decisions matter:
//
//  * The default is chronological, grouped by day, with denied attempts marked. An audit log
//    whose refusals are hidden is not an audit log; the interesting rows are the ones that
//    did not succeed.
//  * Export is a plain link to the server's CSV, not a client-side serialisation. The client
//    must never be the thing that decides what the authoritative record contained.

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

  return (
    <>
      <div className="viewhead">
        <h1>{t("activity.title")}</h1>
        <span className="spacer" />
        <a className="btn sm" href={`/api/audit?format=csv${since ? `&since=${since}` : ""}`} download>
          <Icon name="download" size={12} />
          {t("activity.export")}
        </a>
      </div>

      <div className="filters">
        <label className="sr-only" htmlFor="f-action">
          {t("activity.action")}
        </label>
        <select id="f-action" value={action} onChange={(e) => setAction(e.target.value)}>
          <option value="">{t("activity.allActions")}</option>
          {(page?.actions ?? []).map((a) => (
            <option key={a} value={a}>
              {a}
            </option>
          ))}
        </select>

        <label className="sr-only" htmlFor="f-result">
          {t("activity.result")}
        </label>
        <select id="f-result" value={result} onChange={(e) => setResult(e.target.value)}>
          <option value="">{t("activity.anyResult")}</option>
          <option value="ok">{t("activity.allowed")}</option>
          <option value="denied">{t("activity.refused")}</option>
        </select>

        <label className="sr-only" htmlFor="f-since">
          {t("activity.since")}
        </label>
        <select id="f-since" value={since} onChange={(e) => setSince(e.target.value)}>
          <option value="">{t("activity.allTime")}</option>
          <option value="1h">{t("activity.lastHour")}</option>
          <option value="24h">{t("activity.last24")}</option>
          <option value="7d">{t("activity.last7")}</option>
          <option value="30d">{t("activity.last30")}</option>
        </select>

        <span className="spacer" />
        {page ? (
          <span className="mono upload-count">{t("count.events", { n: page.total })}</span>
        ) : null}
      </div>

      {error ? (
        <ErrorState message={error} onRetry={() => void load(0, true)} />
      ) : page === null ? (
        <Skeleton rows={6} />
      ) : events.length === 0 ? (
        <EmptyState
          icon="pulse"
          title={action || result || since ? t("activity.filtered") : t("activity.empty")}
        />
      ) : (
        <div className="timeline">
          {groups.map(([day, rows]) => (
            <div key={day}>
              <div className="daymark">{day}</div>
              {rows.map((event) => (
                <div key={event.id} className={`ev${event.result === "denied" ? " denied" : ""}`}>
                  <div className="when">
                    <span className="c">{fmt.clock(event.ts)}</span>
                    {fmt.relative(event.ts)}
                  </div>
                  <div className="rail">
                    <span className="dot" />
                  </div>
                  <div className="body">
                    <div className="head">
                      <span className="act">{event.action}</span>
                      <span className="target">{targetOf(event)}</span>
                      <span className="who">· {whoOf(event, t)}</span>
                    </div>
                    <div className="meta">
                      {event.result !== "ok" ? <span>{event.result}</span> : null}
                      {event.bytes ? <span>{fmt.size(event.bytes)}</span> : null}
                      {event.actor_ip ? <span>{event.actor_ip}</span> : null}
                      {event.share_id ? <span>{t("activity.viaLink")}</span> : null}
                    </div>
                  </div>
                </div>
              ))}
            </div>
          ))}

          <div className="loadmore">
            {page?.has_more ? (
              <button
                type="button"
                className="btn"
                disabled={loadingMore}
                onClick={async () => {
                  setLoadingMore(true);
                  await load(events.length, false);
                  setLoadingMore(false);
                }}
              >
                {t("activity.loadOlder")}
              </button>
            ) : (
              <span className="hintline">{t("activity.end")}</span>
            )}
          </div>
        </div>
      )}
    </>
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
