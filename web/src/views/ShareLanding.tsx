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
              {landing.max_uploads ? (
                <p className="hintline">
                  {t("share.slots", {
                    left: Math.max(0, landing.max_uploads - landing.upload_count - uploaded.length),
                    max: landing.max_uploads,
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
            <>
              <a className="btn primary" href={shareDownloadURL(token)}>
                <Icon name="download" size={13} />
                {t("share.download")}
              </a>
              {landing.children && landing.children.length > 0 ? (
                <div className="filelist-simple">
                  {landing.children.map((child) => (
                    <div className="li" key={child.id}>
                      <Icon name={child.kind === "dir" ? "files" : "doc"} size={12} />
                      <span className="n">{child.name}</span>
                      <span className="s">{child.kind === "dir" ? "—" : fmt.size(child.size)}</span>
                    </div>
                  ))}
                </div>
              ) : null}
            </>
          ) : (
            <p className="hintline">{t("share.cannotRead")}</p>
          )}
        </div>

        <div className="foot">{t("app.name")}</div>
      </div>
    </div>
  );
}
