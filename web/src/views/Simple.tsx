// Shared, Trash and Access.
//
// These are deliberately plain tables. They are places people visit to check or undo something,
// not places they work, and giving each one its own bespoke layout would cost attention without
// buying anything. The column browser is where the design budget is spent.

import { useEffect, useState } from "react";
import { api, APIError, sharedContentURL } from "../api";
import { useI18n } from "../i18n";
import { EmptyState, ErrorState, Icon, Skeleton, useToast } from "../ui";
import { NewPasswordDialog } from "../components/Dialogs";
import { FolderAccess } from "../components/FolderAccess";
import type { AppPassword, FileNode, Share, SharedNode } from "../types";

/* ---------- shared ---------- */

/** Shared answers two different questions, so it has two views.
 *
 *  "My links" is what I published. "Shared with me" is what other people let me open. Only the
 *  first existed, which left a whole direction of sharing with no browser entry point: the ACL API
 *  could create a grant on somebody else's folder, and nothing in the product could use it. Both
 *  live here because they are the same concept seen from either end, and a person looking for
 *  "sharing" will look in one place.
 */
export function Shared({ reloadKey }: { reloadKey: number }) {
  const { t } = useI18n();
  const [tab, setTab] = useState<"links" | "received">("links");

  return (
    <>
      <div className="viewhead">
        <h1>{t("shared.title")}</h1>
        <span className="sub">{t("shared.subtitle")}</span>
      </div>

      <div className="adminfilters" role="group" aria-label={t("shared.tabs")}>
        <button
          type="button"
          className={tab === "links" ? "btn primary" : "btn"}
          aria-pressed={tab === "links"}
          onClick={() => setTab("links")}
        >
          {t("shared.tabLinks")}
        </button>
        <button
          type="button"
          className={tab === "received" ? "btn primary" : "btn"}
          aria-pressed={tab === "received"}
          onClick={() => setTab("received")}
        >
          {t("shared.tabReceived")}
        </button>
      </div>

      {tab === "links" ? <OwnedLinks reloadKey={reloadKey} /> : <ReceivedShares reloadKey={reloadKey} />}
    </>
  );
}

/** OwnedLinks is the list of links this account created. */
function OwnedLinks({ reloadKey }: { reloadKey: number }) {
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

  return error ? (
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
                    {share.kind === "request"
                      ? t("home.request")
                      : share.kind === "email"
                        ? t("home.email")
                        : t("home.link")}
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
  );
}

/** A received node, or one of its children.
 *
 *  The listing's roots and the children fetched on expand have the same shape, which is what makes
 *  the tree below recursive.
 */
type SharedEntry = { id: string; name: string; kind: string; size?: number | null };

/** ReceivedShares lists what other accounts granted this caller, and lets them open it.
 *
 *  The permission string is shown rather than implied. It is the authorizer's answer for this
 *  caller, so a person can see why an action is missing instead of finding out by pressing it — and
 *  a revoked grant disappears from the next load because the server recomputes the capability
 *  rather than replaying the stored grant.
 */
function ReceivedShares({ reloadKey }: { reloadKey: number }) {
  const { t } = useI18n();
  const toast = useToast();
  const [items, setItems] = useState<SharedNode[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  // expanded maps a node id to the children fetched for it. `null` records a failed fetch (the
  // grant went away between the listing and the click) so the row can say so instead of rendering
  // an empty folder, which would read as "the owner shared nothing".
  const [expanded, setExpanded] = useState<Record<string, SharedNode["children"] | null>>({});
  const [busy, setBusy] = useState<string | null>(null);

  const load = () => {
    setItems(null);
    setError(null);
    setExpanded({});
    api
      .sharedWithMe()
      .then((res) => setItems(res.items ?? []))
      .catch((err) => setError(err instanceof APIError ? err.message || err.code : String(err)));
  };

  useEffect(load, [reloadKey]);

  const toggle = async (id: string) => {
    if (id in expanded) {
      setExpanded((prev) => {
        const next = { ...prev };
        delete next[id];
        return next;
      });
      return;
    }
    setBusy(id);
    try {
      const full = await api.sharedNode(id);
      setExpanded((prev) => ({ ...prev, [id]: full.children ?? [] }));
    } catch (err) {
      toast.error(err instanceof APIError ? err.message || err.code : String(err));
      setExpanded((prev) => ({ ...prev, [id]: null }));
    } finally {
      setBusy(null);
    }
  };

  return error ? (
    <ErrorState message={error} onRetry={load} />
  ) : items === null ? (
    <Skeleton rows={4} />
  ) : items.length === 0 ? (
    <EmptyState icon="shares" title={t("received.empty")} hint={t("received.emptyHint")} />
  ) : (
    <>
      <div className="filelist-simple">
        {items.map((item) => (
          <ReceivedEntry
            key={item.id}
            node={item}
            permissions={item.permissions}
            expanded={expanded}
            busy={busy}
            onToggle={toggle}
          />
        ))}
      </div>
      {/* The permission string above is the authorizer's answer for the ROOT of each granted subtree.
          Children are re-authorized when opened, so a download button on a child can still be refused —
          saying so is more honest than implying the list is a permission map. */}
      <p className="hintline">{t("received.recheck")}</p>
    </>
  );
}

/** ReceivedEntry renders one row and, when open, its children — recursively.
 *
 *  Recursive because the server authorizes each child individually and answers `/api/shared/{id}`
 *  for any of them, so a nested folder is browsable without a second endpoint. Depth only changes
 *  the indent.
 */
function ReceivedEntry({
  node,
  permissions,
  depth = 0,
  expanded,
  busy,
  onToggle,
}: {
  node: SharedEntry;
  permissions?: string;
  depth?: number;
  expanded: Record<string, SharedNode["children"] | null>;
  busy: string | null;
  onToggle: (id: string) => Promise<void>;
}) {
  const { t, fmt } = useI18n();
  const isDir = node.kind === "dir";
  const open = node.id in expanded;
  const children = expanded[node.id];

  return (
    <>
      <div
        className="li"
        // Indent is computed from the level, so it lives inline; everything else is a class.
        style={{ paddingInlineStart: 9 + depth * 16 }}
      >
        <Icon name={isDir ? "files" : "doc"} size={12} />
        <span className="n" title={node.name}>
          {node.name}
        </span>
        {permissions ? (
          <span className="pill" title={t("received.permissions")}>
            {permissions}
          </span>
        ) : null}
        <span className="s">{isDir ? t("share.folder") : fmt.size(node.size ?? 0)}</span>
        {isDir ? (
          <button
            type="button"
            className="btn sm"
            disabled={busy === node.id}
            aria-expanded={open}
            onClick={() => void onToggle(node.id)}
          >
            {open ? t("received.close") : t("received.open")}
          </button>
        ) : (
          <a
            className="btn sm"
            href={sharedContentURL(node.id)}
            aria-label={t("share.downloadNamed", { name: node.name })}
          >
            <Icon name="download" size={12} />
            {t("received.download")}
          </a>
        )}
      </div>

      {open && children === null ? (
        <div className="li" style={{ paddingInlineStart: 9 + (depth + 1) * 16 }}>
          <span className="n">{t("received.unavailable")}</span>
        </div>
      ) : null}

      {open && children
        ? children.length === 0
          ? (
            <div className="li" style={{ paddingInlineStart: 9 + (depth + 1) * 16 }}>
              <span className="n">{t("received.noChildren")}</span>
            </div>
          )
          : children.map((child) => (
            <ReceivedEntry
              key={child.id}
              node={child}
              depth={depth + 1}
              expanded={expanded}
              busy={busy}
              onToggle={onToggle}
            />
          ))
        : null}
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
