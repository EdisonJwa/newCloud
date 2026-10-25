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
//
// The permission checkboxes are the stored vocabulary rather than a preset, and that is
// deliberate: `write` implies create and overwrite but not delete, so a friendly "can edit"
// would be a silent widening of what the owner chose. The vocabulary is shown, with a sentence
// under it explaining what the unusual ones mean.

import { useEffect, useState } from "react";
import { api, APIError } from "../api";
import { useI18n } from "../i18n";
import { EmptyState, ErrorState, Icon, Skeleton, useToast } from "../ui";
import type { ACLGrant } from "../types";

/** The stored vocabulary, in the order an owner is likely to want them. */
const PERMISSIONS = ["read", "write", "create", "overwrite", "delete", "share", "manage_permissions"] as const;

export function FolderAccess() {
  const { t } = useI18n();
  const toast = useToast();

  const [path, setPath] = useState("");
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
    load("");
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
      toast.success(t("acl.granted"));
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
      toast.success(t("toast.revoked"));
      load(loadedPath ?? path);
    } catch (err) {
      toast.error(err instanceof APIError ? err.message || err.code : String(err));
    }
  };

  return (
    <section aria-labelledby="acl-heading">
      <h2 className="wc-section__title" id="acl-heading">
        {t("acl.title")}
      </h2>
      <p className="serein-hint wc-settings-note">{t("acl.hint")}</p>

      <div className="wc-inlineform">
        <div className="serein-field wc-grow">
          <label className="serein-label" htmlFor="acl-path">
            {t("acl.path")}
          </label>
          <div className="serein-input-wrap serein-input-wrap--sm">
            <input
              id="acl-path"
              className="serein-input"
              type="text"
              value={path}
              placeholder={t("acl.pathPlaceholder")}
              onChange={(e) => setPath(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") {
                  e.preventDefault();
                  load(path);
                }
              }}
            />
          </div>
        </div>
        <button type="button" className="serein-btn serein-btn--outline" onClick={() => load(path)}>
          <Icon name="files" size={13} />
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
        <div className="wc-card">
          <div className="wc-list">
            {grants.map((g) => (
              <div className="wc-list__row" key={g.id}>
                <span className="serein-inspector__callout-icon" aria-hidden="true">
                  <Icon name={g.subject_kind === "group" ? "shares" : "user"} size={14} />
                </span>
                <span className="wc-list__main">
                  <span className="wc-list__title">
                    {g.display || g.subject}
                    <span className="serein-badge serein-badge--outline">{g.subject_kind}</span>
                  </span>
                  <span className="wc-list__meta wc-mono">{g.permissions.join(", ")}</span>
                </span>
                <span className="wc-list__end wc-list__end--secondary">
                  {g.inherit ? t("acl.inherit") : t("acl.thisOnly")}
                </span>
                <button
                  type="button"
                  className="serein-btn serein-btn--sm serein-btn--quiet"
                  onClick={() => void revoke(g)}
                >
                  {t("common.revoke")}
                </button>
              </div>
            ))}
          </div>
        </div>
      )}

      <div className="wc-card wc-card__padding" style={{ marginTop: "var(--serein-space-5)" }}>
        <div className="serein-field">
          <label className="serein-label" htmlFor="acl-subject">
            {t("acl.subject")}
          </label>
          <div className="serein-input-wrap serein-input-wrap--sm">
            <input
              id="acl-subject"
              className="serein-input"
              type="text"
              value={subject}
              placeholder={t("acl.subjectPlaceholder")}
              onChange={(e) => setSubject(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === "Enter") {
                  e.preventDefault();
                  void submit();
                }
              }}
            />
          </div>
        </div>

        <fieldset className="wc-fieldset">
          <legend className="serein-label">{t("acl.permissions")}</legend>
          <div className="wc-checkgrid">
            {PERMISSIONS.map((p) => (
              <label className="serein-check" key={p}>
                <input
                  className="serein-check__input"
                  type="checkbox"
                  checked={perms.includes(p)}
                  onChange={() => togglePerm(p)}
                />
                <span className="serein-check__box" aria-hidden="true" />
                <span className="serein-check__text">
                  <span className="serein-check__label wc-mono">{p}</span>
                </span>
              </label>
            ))}
          </div>
          <p className="serein-hint">{t("acl.permissionsHint")}</p>
        </fieldset>

        <div className="wc-row" style={{ justifyContent: "flex-end" }}>
          <button
            type="button"
            className="serein-btn"
            disabled={busy || !subject.trim() || perms.length === 0}
            onClick={() => void submit()}
          >
            <Icon name="plus" size={13} />
            {t("acl.grant")}
          </button>
        </div>
      </div>
    </section>
  );
}
