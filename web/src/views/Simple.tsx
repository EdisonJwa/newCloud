// Shared, Trash and Access.
//
// These are deliberately plain tables. They are places people visit to check or undo something,
// not places they work, and giving each one its own bespoke layout would cost attention without
// buying anything. The column browser is where the design budget is spent.

import { useEffect, useState } from "react";
import { api, APIError } from "../api";
import { useI18n } from "../i18n";
import { EmptyState, ErrorState, Icon, Skeleton, useToast } from "../ui";
import { NewPasswordDialog } from "../components/Dialogs";
import { FolderAccess } from "../components/FolderAccess";
import type { AppPassword, FileNode, Share } from "../types";

/* ---------- shared links ---------- */

export function Shared({ reloadKey }: { reloadKey: number }) {
  const { t, fmt } = useI18n();
  const toast = useToast();
  const [items, setItems] = useState<Share[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  const load = () => {
    setItems(null);
    setError(null);
    api
      .shares()
      .then((res) => setItems((res.items ?? []).filter((s) => !s.revoked_at)))
      .catch((err) => setError(err instanceof APIError ? err.message || err.code : String(err)));
  };

  useEffect(load, [reloadKey]);

  const revoke = async (id: string) => {
    try {
      await api.revokeShare(id);
      toast.info(t("toast.revoked"));
      load();
    } catch (err) {
      toast.error(err instanceof APIError ? err.message : String(err));
    }
  };

  return (
    <>
      <div className="viewhead">
        <h1>{t("shared.title")}</h1>
        <span className="sub">{t("shared.subtitle")}</span>
      </div>

      {error ? (
        <ErrorState message={error} onRetry={load} />
      ) : items === null ? (
        <Skeleton rows={5} />
      ) : items.length === 0 ? (
        <EmptyState icon="link" title={t("shared.empty")} hint={t("shared.emptyHint")} />
      ) : (
        <div className="tablewrap">
          <table className="table">
            <thead>
              <tr>
                <th>{t("shared.target")}</th>
                <th>{t("shared.kind")}</th>
                <th className="end">{t("shared.uses")}</th>
                <th className="end">{t("shared.expires")}</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {items.map((share) => {
                const max = share.max_downloads ?? share.max_uploads;
                const used = share.download_count + share.upload_count;
                return (
                  <tr key={share.id}>
                    <td>
                      <div className="mono" title={share.node_path}>
                        {share.label || share.node_path}
                      </div>
                    </td>
                    <td>
                      <span className={`pill${share.kind === "request" ? " accent" : ""}`}>
                        {share.kind === "request" ? t("home.request") : t("home.link")}
                      </span>
                      {share.has_password ? <span className="pill flag"> •• </span> : null}
                    </td>
                    <td className="num">{max ? `${used}/${max}` : used}</td>
                    <td className="num">{share.expires_at ? fmt.date(share.expires_at) : t("shared.never")}</td>
                    <td className="end">
                      <button type="button" className="btn sm danger" onClick={() => void revoke(share.id)}>
                        {t("shared.revoke")}
                      </button>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}

/* ---------- trash ---------- */

export function Trash({ reloadKey }: { reloadKey: number }) {
  const { t, fmt } = useI18n();
  const toast = useToast();
  const [items, setItems] = useState<FileNode[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  const load = () => {
    setItems(null);
    setError(null);
    api
      .trash()
      .then((res) => setItems(res.items ?? []))
      .catch((err) => setError(err instanceof APIError ? err.message || err.code : String(err)));
  };

  useEffect(load, [reloadKey]);

  const restore = async (id: string) => {
    try {
      await api.restore(id);
      toast.info(t("toast.restored"));
      load();
    } catch (err) {
      toast.error(err instanceof APIError ? err.message : String(err));
    }
  };

  return (
    <>
      <div className="viewhead">
        <h1>{t("trash.title")}</h1>
        <span className="sub">{t("trash.subtitle")}</span>
      </div>

      {error ? (
        <ErrorState message={error} onRetry={load} />
      ) : items === null ? (
        <Skeleton rows={4} />
      ) : items.length === 0 ? (
        <EmptyState icon="trash" title={t("trash.empty")} hint={t("trash.emptyHint")} />
      ) : (
        <div className="tablewrap">
          <table className="table">
            <thead>
              <tr>
                <th>{t("files.name")}</th>
                <th className="end">{t("files.size")}</th>
                <th className="end">{t("files.modified")}</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {items.map((node) => (
                <tr key={node.id}>
                  <td className="mono">{node.name}</td>
                  <td className="num">{node.kind === "dir" ? "—" : fmt.size(node.size)}</td>
                  <td className="num">{fmt.relative(node.modified_at)}</td>
                  <td className="end">
                    <button type="button" className="btn sm" onClick={() => void restore(node.id)}>
                      {t("trash.restore")}
                    </button>
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

/* ---------- access ---------- */

export function Access({ reloadKey }: { reloadKey: number }) {
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
      toast.info(t("toast.revoked"));
      load();
    } catch (err) {
      toast.error(err instanceof APIError ? err.message : String(err));
    }
  };

  const davURL = `${location.origin}/dav/${encodeURIComponent("")}`.replace(/\/+$/, "/");

  return (
    <>
      <div className="viewhead">
        <h1>{t("access.title")}</h1>
        <span className="spacer" />
        <button type="button" className="btn primary" onClick={() => setCreating(true)}>
          <Icon name="key" size={13} />
          {t("access.newPassword")}
        </button>
      </div>

      <div className="home landing-note">
        <section className="block">
          <h2>{t("access.webdav")}</h2>
          <div className="copybox">{davURL}</div>
          <p className="hintline">{t("access.webdavHint")}</p>
        </section>
      </div>

      <FolderAccess />

      {error ? (
        <ErrorState message={error} onRetry={load} />
      ) : items === null ? (
        <Skeleton rows={3} />
      ) : items.length === 0 ? (
        <EmptyState icon="key" title={t("access.empty")} />
      ) : (
        <div className="tablewrap">
          <table className="table">
            <thead>
              <tr>
                <th>{t("access.label")}</th>
                <th>{t("access.scope")}</th>
                <th>{t("access.lastUsed")}</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {items.map((pw) => (
                <tr key={pw.id}>
                  <td>
                    <div>{pw.label || "—"}</div>
                    <div className="mono upload-count">{pw.prefix}…</div>
                  </td>
                  <td>
                    <span className="pill">{pw.scope}</span>
                  </td>
                  <td className="num">{pw.last_used ? fmt.relative(pw.last_used) : t("access.never")}</td>
                  <td className="end">
                    {pw.revoked ? (
                      <span className="pill flag">revoked</span>
                    ) : (
                      <button type="button" className="btn sm danger" onClick={() => void revoke(pw.id)}>
                        {t("shared.revoke")}
                      </button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {creating ? <NewPasswordDialog onClose={() => setCreating(false)} onDone={load} /> : null}
    </>
  );
}
