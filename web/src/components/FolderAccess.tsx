// Sharing a single folder with another account.
//
// This panel exists because the ACL API had no way in from the browser. The endpoint, the
// authorizer and the "shared with me" view were all built and tested, but nothing in the client
// could create a grant — so the only way to share a folder with a colleague was curl. A
// capability with no route through the UI is a capability most deployments do not have.
//
// The design constraint is that a grant is per node and addressed by path, so the panel is
// anchored to one folder at a time rather than listing every grant the account owns. That is the
// server's model (ACL rows hang off a node), and flattening it into an estate-wide list here
// would invent a view the API does not have.

import { useEffect, useState } from "react";
import { api, APIError } from "../api";
import { useI18n } from "../i18n";
import { EmptyState, ErrorState, Icon, Skeleton, useToast } from "../ui";
import type { ACLGrant } from "../types";

// The stored vocabulary. `write` implies create and overwrite but not delete, so `delete` is
// offered separately rather than folded in: an owner granting edit access rarely means "and you
// may destroy it".
const PERMISSIONS = ["read", "write", "create", "overwrite", "delete", "share", "manage_permissions"] as const;

export function FolderAccess() {
  const { t } = useI18n();
  const toast = useToast();

  const [path, setPath] = useState("/");
  const [loadedPath, setLoadedPath] = useState<string | null>(null);
  const [grants, setGrants] = useState<ACLGrant[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  const [subject, setSubject] = useState("");
  const [perms, setPerms] = useState<string[]>(["read"]);
  const [busy, setBusy] = useState(false);

  const load = (target: string) => {
    setGrants(null);
    setError(null);
    api
      .acl(target)
      .then((res) => {
        setGrants(res?.grants ?? []);
        setLoadedPath(target);
      })
      .catch((err) => setError(err instanceof APIError ? err.message || err.code : String(err)));
  };

  useEffect(() => {
    load(loadedPath ?? "/");
    // Only on mount: re-loading on every keystroke would hammer the endpoint, and the explicit
    // Load button is the intended trigger.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const togglePerm = (p: string) => {
    setPerms((cur) => (cur.includes(p) ? cur.filter((x) => x !== p) : [...cur, p]));
  };

  const submit = async () => {
    if (!subject.trim() || perms.length === 0) return;
    setBusy(true);
    try {
      await api.grant({ path: loadedPath ?? path, subject: subject.trim(), permissions: perms });
      toast.info(t("acl.granted"));
      setSubject("");
      load(loadedPath ?? path);
    } catch (err) {
      // The server distinguishes an unknown subject (404) from a bad permission (400); passing
      // its message through is more useful than a generic failure.
      toast.error(err instanceof APIError ? err.message || err.code : String(err));
    } finally {
      setBusy(false);
    }
  };

  const revoke = async (g: ACLGrant) => {
    try {
      await api.revokeGrant(g.id, loadedPath ?? path);
      toast.info(t("toast.revoked"));
      load(loadedPath ?? path);
    } catch (err) {
      toast.error(err instanceof APIError ? err.message || err.code : String(err));
    }
  };

  return (
    <section className="block" aria-labelledby="acl-heading">
      <h2 id="acl-heading">{t("acl.title")}</h2>
      <p className="hintline">{t("acl.hint")}</p>

      <div className="aclbar">
        <label htmlFor="acl-path" className="sr-only">
          {t("acl.path")}
        </label>
        <input
          id="acl-path"
          type="text"
          value={path}
          placeholder="/documents"
          onChange={(e) => setPath(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") load(path);
          }}
        />
        <button type="button" className="btn" onClick={() => load(path)}>
          <Icon name="folder" size={13} />
          {t("acl.load")}
        </button>
      </div>

      {error ? (
        <ErrorState message={error} onRetry={() => load(loadedPath ?? path)} />
      ) : grants === null ? (
        <Skeleton rows={2} />
      ) : grants.length === 0 ? (
        <EmptyState icon="key" title={t("acl.empty")} hint={t("acl.emptyHint")} />
      ) : (
        <div className="tablewrap">
          <table className="table">
            <thead>
              <tr>
                <th>{t("acl.subject")}</th>
                <th>{t("acl.permissions")}</th>
                <th>{t("acl.scope")}</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {grants.map((g) => (
                <tr key={g.id}>
                  <td>
                    {g.display || g.subject}
                    <span className="tag">{g.subject_kind}</span>
                  </td>
                  <td className="mono">{g.permissions.join(", ")}</td>
                  <td>{g.inherit ? t("acl.inherit") : t("acl.thisOnly")}</td>
                  <td className="end">
                    <button type="button" className="btn ghost" onClick={() => revoke(g)}>
                      {t("common.revoke")}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <div className="aclform">
        <div className="aclformrow">
          <label htmlFor="acl-subject">{t("acl.subject")}</label>
          <input
            id="acl-subject"
            type="text"
            value={subject}
            placeholder={t("acl.subjectPlaceholder")}
            onChange={(e) => setSubject(e.target.value)}
          />
        </div>
        <fieldset className="aclperms">
          <legend>{t("acl.permissions")}</legend>
          {PERMISSIONS.map((p) => (
            <label key={p} className="check">
              <input
                type="checkbox"
                checked={perms.includes(p)}
                onChange={() => togglePerm(p)}
              />
              {p}
            </label>
          ))}
        </fieldset>
        <button
          type="button"
          className="btn primary"
          disabled={busy || !subject.trim() || perms.length === 0}
          onClick={submit}
        >
          <Icon name="plus" size={13} />
          {t("acl.grant")}
        </button>
      </div>
    </section>
  );
}
