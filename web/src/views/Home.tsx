// Home — the landing surface.
//
// This is the half of the product the column browser cannot answer: "what was I doing?". It is
// built from real data rather than dashboard decoration — every number is actionable and every
// tile opens the thing it describes.
//
// The three blocks are ordered by how often they are the reason somebody opened the app:
// continue, then the links they handed out, then the state of the space.

import { useEffect, useState } from "react";
import { api, APIError, thumbnailURL } from "../api";
import { useI18n } from "../i18n";
import { EmptyState, ErrorState, Icon, Skeleton } from "../ui";
import type { AuditPage, FileNode, Me, Share } from "../types";

interface Props {
  me: Me | null;
  onOpenFiles: (path: string) => void;
  onOpenShared: () => void;
  reloadKey: number;
}

export function Home({ me, onOpenFiles, onOpenShared, reloadKey }: Props) {
  const { t, fmt } = useI18n();
  const [recent, setRecent] = useState<FileNode[] | null>(null);
  const [shares, setShares] = useState<Share[] | null>(null);
  const [events, setEvents] = useState<AuditPage | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let live = true;
    setError(null);

    Promise.all([api.recent(8), api.shares(), api.audit({ offset: 0 })])
      .then(([recents, shareList, audit]) => {
        if (!live) return;
        setRecent(recents.items ?? []);
        setShares((shareList.items ?? []).filter((s) => !s.revoked_at));
        setEvents(audit);
      })
      .catch((err) => {
        if (!live) return;
        setError(err instanceof APIError ? err.message || err.code : String(err));
        setRecent([]);
        setShares([]);
      });

    return () => {
      live = false;
    };
  }, [reloadKey]);

  if (error) {
    return <ErrorState message={error} onRetry={() => setRecent(null)} />;
  }

  const quota = me?.quota_bytes ?? 0;
  const used = me?.used_bytes ?? 0;
  const percent = quota > 0 ? Math.min(100, Math.round((used / quota) * 100)) : 0;
  const activeShares = shares ?? [];

  return (
    <div className="home">
      <section className="block">
        <h2>
          <span>{t("home.continue")}</span>
          <span className="spacer" />
          <button type="button" className="link" onClick={() => onOpenFiles("")}>
            {t("home.openFiles")}
          </button>
        </h2>

        {recent === null ? (
          <Skeleton rows={3} />
        ) : recent.length === 0 ? (
          <EmptyState
            icon="upload"
            title={t("home.continueEmpty")}
            actions={
              <button type="button" className="btn primary" onClick={() => onOpenFiles("")}>
                {t("files.upload")}
              </button>
            }
          />
        ) : (
          <div className="recents">
            {recent.map((file) => (
              <button
                key={file.id}
                type="button"
                className="recent"
                onClick={() => onOpenFiles(dirOf(file.path))}
                title={file.path}
              >
                <span className="thumb">
                  {file.preview === "image" ? (
                    <img src={thumbnailURL(file.path)} alt="" loading="lazy" />
                  ) : (
                    <Icon name={file.preview === "text" ? "doc" : "files"} size={18} />
                  )}
                </span>
                <span className="meta">
                  <span className="n">{file.name}</span>
                  <span className="s">
                    {fmt.size(file.size)} · {fmt.relative(file.modified_at)}
                  </span>
                </span>
              </button>
            ))}
          </div>
        )}
      </section>

      <section className="block">
        <h2>
          <span>{t("home.sharedByYou")}</span>
          <span className="spacer" />
          {activeShares.length > 0 ? (
            <button type="button" className="link" onClick={onOpenShared}>
              {t("nav.shared")}
            </button>
          ) : null}
        </h2>

        {shares === null ? (
          <Skeleton rows={2} />
        ) : activeShares.length === 0 ? (
          <EmptyState icon="link" title={t("home.sharedEmpty")} hint={t("shared.emptyHint")} />
        ) : (
          <div className="shares">
            {activeShares.slice(0, 6).map((share) => {
              const used = share.download_count + share.upload_count;
              const max = share.max_downloads ?? share.max_uploads;
              return (
                <div className="sharecard" key={share.id}>
                  <span className="glyph">
                    <Icon name={share.kind === "request" ? "inbox" : "link"} size={13} />
                  </span>
                  <span className="body">
                    <span className="t">{share.label || share.node_path}</span>
                    <span className="m">
                      <span>{share.kind === "request" ? t("home.request") : t("home.link")}</span>
                      <span>
                        {max ? t("home.uses", { used, max }) : t("home.unlimited")}
                      </span>
                      {share.expires_at ? (
                        <span>{t("home.expires", { date: fmt.date(share.expires_at) })}</span>
                      ) : null}
                    </span>
                  </span>
                </div>
              );
            })}
          </div>
        )}
      </section>

      <section className="block">
        <h2>
          <span>{t("home.space")}</span>
        </h2>
        <div className="facts">
          <div className="fact">
            <div className="k">{t("home.used")}</div>
            <div className="v">
              {fmt.size(used)}
              {quota > 0 ? <small> / {fmt.size(quota)}</small> : null}
            </div>
            {quota > 0 ? (
              <div className="meter">
                <progress className="meter" max={100} value={percent} aria-label={t("home.used")} />
              </div>
            ) : null}
          </div>
          <div className="fact">
            <div className="k">{t("home.quota")}</div>
            <div className="v">{quota > 0 ? fmt.size(quota) : t("home.unlimited")}</div>
          </div>
          <div className="fact">
            <div className="k">{t("nav.shared")}</div>
            <div className="v">{activeShares.length}</div>
          </div>
          <div className="fact">
            <div className="k">{t("nav.activity")}</div>
            <div className="v">
              {events && events.events.length > 0 ? (
                <span className="small">{fmt.relative(events.events[0]?.ts ?? "")}</span>
              ) : (
                "—"
              )}
            </div>
          </div>
        </div>
      </section>
    </div>
  );
}

function dirOf(path: string): string {
  const clean = path.replace(/^\//, "");
  const i = clean.lastIndexOf("/");
  return i < 0 ? "" : clean.slice(0, i);
}
