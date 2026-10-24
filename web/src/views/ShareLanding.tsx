// Public share landing page.
//
// Unauthenticated by design: whoever holds the link is the audience. The page therefore shows
// as little as it can while still being usable — no file tree for a link share, no owner name,
// no counts that would let somebody enumerate. A "drop box" link shows only the upload form and
// the remaining slot count.

import { useEffect, useState } from "react";
import {
  APIError,
  fetchShareLanding,
  requestShareCode,
  shareDownloadURL,
  uploadToShare,
  verifyShareCode,
  verifySharePassword,
} from "../api";
import { useI18n } from "../i18n";
import { Icon, useToast } from "../ui";
import type { ShareLanding as Landing } from "../types";

export function ShareLanding({ token }: { token: string }) {
  const { t, fmt } = useI18n();
  const toast = useToast();
  const [landing, setLanding] = useState<Landing | null>(null);
  const [unavailable, setUnavailable] = useState(false);
  const [password, setPassword] = useState("");
  const [email, setEmail] = useState("");
  const [code, setCode] = useState("");
  const [codeSent, setCodeSent] = useState(false);
  const [wrong, setWrong] = useState("");
  const [busy, setBusy] = useState(false);
  const [uploaded, setUploaded] = useState<string[]>([]);

  const load = () => {
    fetchShareLanding(token)
      .then(setLanding)
      .catch(() => setUnavailable(true));
  };

  useEffect(load, [token]);

  if (unavailable) {
    return (
      <div className="landing">
        <div className="landcard">
          <div className="head">
            <span className="mark" aria-hidden="true" />
            <h1>{t("share.notAvailable")}</h1>
            <p>{t("share.notAvailableHint")}</p>
          </div>
          <div className="foot">{t("app.name")}</div>
        </div>
      </div>
    );
  }

  if (!landing) {
    return (
      <div className="landing">
        <div className="landcard">
          <div className="body">{t("state.loading")}</div>
        </div>
      </div>
    );
  }

  const needsPassword = landing.requires_password;
  const needsEmail = landing.requires_email;

  // The payload differs by state, so a drop box's counters are absent on a plain link. Defaulting
  // them once keeps the arithmetic below from guarding at every site.
  const uploadCount = landing.upload_count ?? 0;
  const maxUploads = landing.max_uploads ?? 0;

  // root_kind decides which primary action is honest.
  //
  // A shared DIRECTORY has no bytes of its own: `GET /s/{token}/download` answers 400
  // `is_directory` for it, and a child is fetched with `?name=`. Rendering one Download button for
  // both kinds therefore gave every folder share a primary action that was guaranteed to fail —
  // and a shared folder is the common case. When a payload omits root_kind (an older server), a
  // non-empty child list still identifies a directory.
  const isFolder = landing.root_kind ? landing.root_kind === "dir" : (landing.children?.length ?? 0) > 0;

  return (
    <div className="landing">
      <div className="landcard">
        <div className="head">
          <span className="mark" aria-hidden="true" />
          <h1>{landing.label || t("app.name")}</h1>
          <p>
            {landing.kind === "request" ? t("dlg.shareRequest") : t("share.sharedWith")}
            {landing.expires_at ? ` · ${t("home.expires", { date: fmt.date(landing.expires_at) })}` : ""}
          </p>
        </div>

        <div className="body">
          {needsPassword ? (
            <form
              onSubmit={async (e) => {
                e.preventDefault();
                setBusy(true);
                const ok = await verifySharePassword(token, password);
                setBusy(false);
                if (ok) {
                  setWrong("");
                  load();
                } else {
                  setWrong(t("share.wrongPassword"));
                }
              }}
            >
              <p className="hintline">{t("share.passwordProtected")}</p>
              <label className="field">
                <span className="micro">{t("share.password")}</span>
                <input
                  type="password"
                  value={password}
                  autoFocus
                  onChange={(e) => setPassword(e.target.value)}
                />
              </label>
              {wrong ? <p className="hintline">{wrong}</p> : null}
              <button type="submit" className="btn primary" disabled={busy || !password}>
                {t("share.continue")}
              </button>
            </form>
          ) : needsEmail ? (
            <form
              onSubmit={async (e) => {
                e.preventDefault();
                setBusy(true);
                const ok = await verifyShareCode(token, email, code);
                setBusy(false);
                if (ok) {
                  setWrong("");
                  load();
                } else {
                  setWrong(t("share.codeBad"));
                }
              }}
            >
              <p className="hintline">{t("share.emailLimited")}</p>
              <label className="field">
                <span className="micro">{t("share.email")}</span>
                <input type="email" value={email} autoFocus onChange={(e) => setEmail(e.target.value)} />
              </label>
              {codeSent ? (
                <label className="field">
                  <span className="micro">{t("share.code")}</span>
                  <input value={code} onChange={(e) => setCode(e.target.value)} />
                </label>
              ) : null}
              {wrong ? <p className="hintline">{wrong}</p> : null}
              {codeSent ? (
                <button type="submit" className="btn primary" disabled={busy || !code}>
                  {t("share.continue")}
                </button>
              ) : (
                <button
                  type="button"
                  className="btn primary"
                  disabled={busy || !email}
                  onClick={async () => {
                    setBusy(true);
                    await requestShareCode(token, email);
                    setBusy(false);
                    setCodeSent(true);
                    toast.info(t("share.codeSent"));
                  }}
                >
                  {t("share.sendCode")}
                </button>
              )}
            </form>
          ) : landing.kind === "request" ? (
            <>
              <p className="hintline">{t("share.uploadOne")}</p>
              <label className="field">
                <span className="micro">{t("share.file")}</span>
                <input
                  type="file"
                  multiple
                  onChange={async (e) => {
                    const files = Array.from(e.target.files ?? []);
                    e.target.value = "";
                    for (const file of files) {
                      try {
                        const name = await uploadToShare(token, file);
                        setUploaded((prev) => [...prev, name]);
                      } catch (err) {
                        toast.error(err instanceof APIError ? err.message : String(err));
                      }
                    }
                  }}
                />
              </label>
              {maxUploads > 0 ? (
                <p className="hintline">
                  {t("share.slots", {
                    left: Math.max(0, maxUploads - uploadCount - uploaded.length),
                    max: maxUploads,
                  })}
                </p>
              ) : null}
              {uploaded.length > 0 ? (
                <div className="filelist-simple">
                  {uploaded.map((name) => (
                    <div className="li" key={name}>
                      <Icon name="doc" size={12} />
                      <span className="n">{name}</span>
                    </div>
                  ))}
                </div>
              ) : null}
            </>
          ) : landing.can_read ? (
            isFolder ? (
              <>
                <p className="hintline">{t("share.folderHint")}</p>
                {landing.children && landing.children.length > 0 ? (
                  <div className="filelist-simple">
                    {landing.children.map((child) => (
                      <div className="li" key={child.id}>
                        <Icon name={child.kind === "dir" ? "files" : "doc"} size={12} />
                        <span className="n">{child.name}</span>
                        <span className="s">{child.kind === "dir" ? t("share.folder") : fmt.size(child.size)}</span>
                        {/* Only files carry bytes. A nested folder is listed so the recipient can
                            see the shape of what was shared, but it is not an action: this route
                            serves a child FILE by name, and offering a link that 400s would
                            reintroduce the defect this branch exists to fix. */}
                        {child.kind === "dir" ? null : (
                          <a
                            className="btn sm"
                            href={shareDownloadURL(token, child.name)}
                            aria-label={t("share.downloadNamed", { name: child.name })}
                          >
                            <Icon name="download" size={12} />
                            {t("share.download")}
                          </a>
                        )}
                      </div>
                    ))}
                  </div>
                ) : (
                  <p className="hintline">{t("share.folderEmpty")}</p>
                )}
              </>
            ) : (
              <a className="btn primary" href={shareDownloadURL(token)}>
                <Icon name="download" size={13} />
                {t("share.download")}
              </a>
            )
          ) : (
            <p className="hintline">{t("share.cannotRead")}</p>
          )}
        </div>

        <div className="foot">{t("app.name")}</div>
      </div>
    </div>
  );
}
