// Public share landing page.
//
// Unauthenticated by design: whoever holds the link is the audience. The page therefore shows
// as little as it can while still being usable — no file tree for a link share, no owner name,
// no counts that would let somebody enumerate. A "drop box" link shows only the upload form and
// the remaining slot count.
//
// ## The gate discloses nothing
//
// The password gate is deliberately generic. It does not print the share's label, the target
// name, the item count, a thumbnail or a content description, because all of that is protected
// content and printing it above a password field would make the password ornamental. The
// interface says "protected share", the field, the button, and one privacy line. A wrong
// password gets one accessible alert and no attempt counter: nothing here enforces a lockout,
// and claiming a remaining-attempts budget would simulate a security guarantee that does not
// exist. Rate limiting, lockout and verification belong on the server.
//
// ## The page has its own appearance
//
// A recipient is not signed in and has no account menu, so the theme switch lives in this
// page's own footer. It writes the same shared preference as the app, which means a person who
// chose dark keeps dark across the boundary.

import { useEffect, useMemo, useState } from "react";
import {
  APIError,
  fetchShareLanding,
  requestShareCode,
  shareDownloadURL,
  uploadToShare,
  verifyShareCode,
  verifySharePassword,
} from "../api";
import { useI18n, LANGS, type Lang } from "../i18n";
import { Icon, FileTile, useToast, extensionOf } from "../ui";
import type { ShareLanding as Landing } from "../types";
import type { Prefs, ThemePref } from "../appearance";

type Mode = "password" | "content" | "upload" | "unavailable";

export function ShareLanding({
  token,
  lang,
  onLang,
  prefs,
  onPrefs,
}: {
  token: string;
  lang: Lang;
  onLang: (lang: Lang) => void;
  prefs: Prefs;
  onPrefs: (next: Prefs) => void;
}) {
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
    setUnavailable(false);
    fetchShareLanding(token)
      .then(setLanding)
      .catch(() => setUnavailable(true));
  };

  useEffect(load, [token]);

  const needsPassword = landing?.requires_password ?? false;
  const needsEmail = landing?.requires_email ?? false;

  // The payload differs by state, so a drop box's counters are absent on a plain link.
  // Defaulting them once keeps the arithmetic below from guarding at every site.
  const uploadCount = landing?.upload_count ?? 0;
  const maxUploads = landing?.max_uploads ?? 0;

  // root_kind decides which primary action is honest.
  //
  // A shared DIRECTORY has no bytes of its own: `GET /s/{token}/download` answers 400
  // `is_directory` for it, and a child is fetched with `?name=`. Rendering one Download button
  // for both kinds therefore gave every folder share a primary action that was guaranteed to
  // fail. When a payload omits root_kind (an older server), a non-empty child list still
  // identifies a directory.
  const isFolder = landing
    ? landing.root_kind
      ? landing.root_kind === "dir"
      : (landing.children?.length ?? 0) > 0
    : false;

  const mode: Mode = unavailable
    ? "unavailable"
    : !landing || needsPassword || needsEmail
      ? "password"
      : landing.kind === "request"
        ? "upload"
        : "content";

  const title = useMemo(() => {
    if (mode === "unavailable") return t("share.notAvailable");
    if (mode === "password") return needsEmail ? t("share.emailHeading") : t("share.gateHeading");
    if (mode === "upload") return landing?.label || t("share.uploadHeading");
    return landing?.label || landing?.root_name || t("share.contentHeading");
  }, [mode, needsEmail, landing, t]);

  return (
    <div className="serein-public" data-mode={mode}>
      <header className="serein-public__header">
        <span className="serein-public__brand">
          <span className="serein-brand__mark" aria-hidden="true">
            <BrandGlyph />
          </span>
          {t("app.name")}
        </span>
        <div className="serein-public__owner">
          {mode === "content" && landing?.root_kind === "dir" ? (
            <span>{t("share.sharedFolder")}</span>
          ) : mode === "upload" ? (
            <span>{t("share.dropBox")}</span>
          ) : null}
        </div>
      </header>

      <main className="serein-public__main" id="main">
        {unavailable ? (
          <section className="serein-password serein-password--unlocked" aria-labelledby="landing-title">
            <span className="serein-password__icon">
              <Icon name="alert" size={22} />
            </span>
            <h1 className="serein-password__title" id="landing-title">
              {title}
            </h1>
            <p className="serein-password__description">{t("share.notAvailableHint")}</p>
          </section>
        ) : !landing ? (
          <p className="serein-hint">
            <span className="serein-spinner" aria-hidden="true" /> {t("state.loading")}
          </p>
        ) : needsPassword ? (
          <form
            className="serein-password"
            aria-labelledby="gate-title"
            onSubmit={async (e) => {
              e.preventDefault();
              setBusy(true);
              // try/finally, not a trailing setBusy(false): a network rejection would leave
              // the gate permanently "busy", which reads as a broken Continue button.
              let ok = false;
              try {
                ok = await verifySharePassword(token, password);
              } finally {
                setBusy(false);
              }
              if (ok) {
                setWrong("");
                load();
              } else {
                setWrong(t("share.wrongPassword"));
              }
            }}
          >
            <span className="serein-password__icon">
              <Icon name="key" size={22} />
            </span>
            <p className="serein-password__eyebrow">{t("share.gateEyebrow")}</p>
            <h1 className="serein-password__title" id="gate-title">
              {t("share.gateHeading")}
            </h1>
            <p className="serein-password__description">{t("share.gateBlurb")}</p>

            <div className="serein-password__form">
              <div className="serein-password__row">
                <div className="serein-field">
                  <label className="serein-label" htmlFor="gate-password">
                    {t("share.password")}
                  </label>
                  <div className="serein-input-wrap">
                    <input
                      id="gate-password"
                      className="serein-input"
                      type="password"
                      value={password}
                      autoComplete="current-password"
                      spellCheck={false}
                      autoFocus
                      aria-describedby={wrong ? "gate-error" : undefined}
                      onChange={(e) => setPassword(e.target.value)}
                    />
                  </div>
                </div>
                <button type="submit" className="serein-btn" disabled={busy || !password}>
                  {busy ? <span className="serein-spinner" aria-hidden="true" /> : null}
                  {t("share.continue")}
                </button>
              </div>
              {wrong ? (
                <p className="serein-field__error" id="gate-error" role="alert">
                  <Icon name="alert" size={12} />
                  {wrong}
                </p>
              ) : null}
            </div>

            <hr className="serein-password__rule" />
            <p className="serein-password__privacy">
              <Icon name="eye" size={12} />
              {t("share.gatePrivacy")}
            </p>
          </form>
        ) : needsEmail ? (
          <form
            className="serein-password"
            aria-labelledby="email-title"
            onSubmit={async (e) => {
              e.preventDefault();
              setBusy(true);
              let ok = false;
              try {
                ok = await verifyShareCode(token, email, code);
              } finally {
                setBusy(false);
              }
              if (ok) {
                setWrong("");
                load();
              } else {
                setWrong(t("share.codeBad"));
              }
            }}
          >
            <span className="serein-password__icon">
              <Icon name="user" size={22} />
            </span>
            <p className="serein-password__eyebrow">{t("share.gateEyebrow")}</p>
            <h1 className="serein-password__title" id="email-title">
              {t("share.emailHeading")}
            </h1>
            <p className="serein-password__description">{t("share.emailLimited")}</p>

            <div className="serein-password__form">
              <div className="serein-field">
                <label className="serein-label" htmlFor="gate-email">
                  {t("share.email")}
                </label>
                <div className="serein-input-wrap">
                  <input
                    id="gate-email"
                    className="serein-input"
                    type="email"
                    value={email}
                    autoComplete="email"
                    spellCheck={false}
                    autoFocus
                    aria-describedby={wrong ? "email-error" : undefined}
                    onChange={(e) => setEmail(e.target.value)}
                  />
                </div>
              </div>
              {codeSent ? (
                <div className="serein-field">
                  <label className="serein-label" htmlFor="gate-code">
                    {t("share.code")}
                  </label>
                  <div className="serein-input-wrap">
                    <input
                      id="gate-code"
                      className="serein-input"
                      value={code}
                      inputMode="numeric"
                      autoComplete="one-time-code"
                      spellCheck={false}
                      aria-describedby={wrong ? "email-error" : undefined}
                      onChange={(e) => setCode(e.target.value)}
                    />
                  </div>
                </div>
              ) : null}

              {wrong ? (
                <p className="serein-field__error" id="email-error" role="alert">
                  <Icon name="alert" size={12} />
                  {wrong}
                </p>
              ) : null}

              {codeSent ? (
                <button type="submit" className="serein-btn serein-btn--block" disabled={busy || !code}>
                  {t("share.continue")}
                </button>
              ) : (
                <button
                  type="button"
                  className="serein-btn serein-btn--block"
                  disabled={busy || !email}
                  onClick={async () => {
                    setBusy(true);
                    try {
                      await requestShareCode(token, email);
                      setCodeSent(true);
                      toast.info(t("share.codeSent"));
                    } catch (err) {
                      toast.error(err instanceof APIError ? err.message || err.code : String(err));
                    } finally {
                      setBusy(false);
                    }
                  }}
                >
                  {t("share.sendCode")}
                </button>
              )}
            </div>

            <hr className="serein-password__rule" />
            <p className="serein-password__privacy">
              <Icon name="eye" size={12} />
              {t("share.gatePrivacy")}
            </p>
          </form>
        ) : landing.kind === "request" ? (
          <section className="serein-upload-card" aria-labelledby="upload-title">
            <p className="serein-upload-card__eyebrow">{t("share.dropBox")}</p>
            <h1 className="serein-upload-card__title" id="upload-title">
              {landing.label || t("share.uploadHeading")}
            </h1>
            <p className="serein-upload-card__intro">{t("share.uploadOne")}</p>

            <label className="serein-dropzone">
              <span className="serein-dropzone__icon">
                <Icon name="upload" size={26} />
              </span>
              <span className="serein-dropzone__title">{t("share.chooseFiles")}</span>
              <span className="serein-dropzone__hint">{t("share.chooseFilesHint")}</span>
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
                      toast.success(t("share.uploaded", { name }));
                    } catch (err) {
                      toast.error(err instanceof APIError ? err.message || err.code : String(err));
                    }
                  }
                }}
              />
            </label>

            {maxUploads > 0 ? (
              <p className="serein-hint wc-upload-slots">
                {t("share.slots", {
                  left: Math.max(0, maxUploads - uploadCount - uploaded.length),
                  max: maxUploads,
                })}
              </p>
            ) : null}

            {uploaded.length > 0 ? (
              <div className="serein-upload-list">
                {uploaded.map((name) => (
                  <div className="serein-upload-item is-complete" key={name}>
                    <FileTile
                      entry={{ kind: "file", preview: undefined, name, path: "" }}
                      size={28}
                    />
                    <span className="serein-upload-item__main">
                      <span className="serein-upload-item__name">{name}</span>
                      <span className="serein-upload-item__meta">{t("share.received")}</span>
                    </span>
                  </div>
                ))}
              </div>
            ) : null}
          </section>
        ) : landing.can_read ? (
          isFolder ? (
            <div className="wc-public">
              <div className="serein-public__hero">
                <div>
                  <p className="serein-public__eyebrow">{t("share.folderEyebrow")}</p>
                  <h1 className="serein-public__title">{landing.label || t("share.folder")}</h1>
                  <p className="serein-public__lede">{t("share.folderHint")}</p>
                  <div className="serein-public__actions">
                    {landing.children && landing.children.length > 0 ? (
                      <a
                        className="serein-btn serein-btn--outline"
                        href={shareDownloadURL(token, undefined)}
                        aria-disabled="true"
                        onClick={(e) => e.preventDefault()}
                      >
                        <Icon name="download" size={13} />
                        {t("share.downloadAllSoon")}
                      </a>
                    ) : null}
                  </div>
                  <p className="serein-public__meta">
                    <span>{t("share.itemCount", { n: landing.children?.length ?? 0 })}</span>
                    {landing.expires_at ? (
                      <span>{t("home.expires", { date: fmt.date(landing.expires_at) })}</span>
                    ) : null}
                  </p>
                </div>
                <div className="serein-public__mosaic" aria-hidden="true">
                  {(landing.children ?? []).slice(0, 4).map((child) => (
                    <span className="serein-public__mosaic-tile" key={child.id}>
                      <FileTile
                        entry={{
                          kind: child.kind === "dir" ? "dir" : "file",
                          preview: undefined,
                          name: child.name,
                          path: "",
                        }}
                        size={64}
                      />
                    </span>
                  ))}
                </div>
              </div>

              <div className="serein-public__files">
                <div className="serein-public__files-head">
                  <h2>{t("share.filesHeading")}</h2>
                  <span>{t("share.itemCount", { n: landing.children?.length ?? 0 })}</span>
                </div>
                {landing.children && landing.children.length > 0 ? (
                  landing.children.map((child) => (
                    <div className="serein-public__file" key={child.id}>
                      <FileTile
                        entry={{
                          kind: child.kind === "dir" ? "dir" : "file",
                          preview: undefined,
                          name: child.name,
                          path: "",
                        }}
                        size={34}
                      />
                      <div className="serein-public__file-main">
                        <span className="serein-public__file-name">{child.name}</span>
                        <span className="serein-public__file-meta">
                          {child.kind === "dir"
                            ? t("share.folder")
                            : `${extensionOf(child.name).toUpperCase()} · ${fmt.size(child.size ?? 0)}`}
                        </span>
                      </div>
                      {/* Only files carry bytes. A nested folder is listed so the recipient can
                          see the shape of what was shared, but it is not an action: this route
                          serves a child FILE by name, and offering a link that 400s would
                          reintroduce the defect this branch exists to fix. */}
                      {child.kind === "dir" ? (
                        <span className="serein-badge serein-badge--outline">{t("share.folder")}</span>
                      ) : (
                        <a
                          className="serein-btn serein-btn--sm serein-btn--outline"
                          href={shareDownloadURL(token, child.name)}
                          aria-label={t("share.downloadNamed", { name: child.name })}
                          download
                        >
                          <Icon name="download" size={12} />
                          <span className="wc-label">{t("share.download")}</span>
                        </a>
                      )}
                    </div>
                  ))
                ) : (
                  <p className="serein-hint">{t("share.folderEmpty")}</p>
                )}
              </div>
            </div>
          ) : (
            <div className="wc-public">
              <div className="serein-public__hero">
                <div>
                  <p className="serein-public__eyebrow">{t("share.fileEyebrow")}</p>
                  <h1 className="serein-public__title">{landing.label || landing.root_name || t("share.file")}</h1>
                  <p className="serein-public__lede">{t("share.fileLede")}</p>
                  <div className="serein-public__actions">
                    <a className="serein-btn serein-btn--lg" href={shareDownloadURL(token)} download>
                      <Icon name="download" size={15} />
                      {t("share.download")}
                    </a>
                  </div>
                  <p className="serein-public__meta">
                    {landing.expires_at ? (
                      <span>{t("home.expires", { date: fmt.date(landing.expires_at) })}</span>
                    ) : null}
                  </p>
                </div>
                <div className="serein-public__mosaic">
                  <span className="serein-public__mosaic-tile">
                    <FileTile
                      entry={{
                        kind: "file",
                        preview: undefined,
                        name: landing.root_name || landing.label || "file",
                        path: "",
                      }}
                      size={120}
                    />
                  </span>
                </div>
              </div>
            </div>
          )
        ) : (
          <section className="serein-password serein-password--unlocked">
            <span className="serein-password__icon">
              <Icon name="alert" size={22} />
            </span>
            <h1 className="serein-password__title">{t("share.cannotRead")}</h1>
          </section>
        )}
      </main>

      <footer className="serein-public__footer">
        <span className="serein-public__protected">
          <Icon name="eye" size={11} />
          {t("share.footerPrivacy")}
        </span>
        <div className="serein-public__footer-links">
          <ThemeSwitch prefs={prefs} onPrefs={onPrefs} />
          <button type="button" className="serein-link serein-link--subtle" onClick={() => onLang(lang === "en" ? "zh-Hans" : "en")}>
            {lang === "en" ? "中文" : "English"}
          </button>
          <span>{t("app.name")}</span>
        </div>
      </footer>
    </div>
  );
}

function BrandGlyph() {
  return (
    <svg viewBox="0 0 24 24" fill="none" aria-hidden="true" focusable="false">
      <path
        d="M7 16.5h9.6a3.1 3.1 0 0 0 .5-6.15A4.6 4.6 0 0 0 8.2 9.1 3.6 3.6 0 0 0 7 16.5Z"
        fill="currentColor"
        opacity="0.92"
      />
      <path
        d="M6.4 19.4c2.6 0 4.2-1.3 5.6-1.3s3 1.3 5.6 1.3"
        stroke="currentColor"
        strokeWidth="1.5"
        strokeLinecap="round"
        opacity="0.55"
      />
    </svg>
  );
}

/**
 * ThemeSwitch is the recipient page's own appearance control.
 *
 * It cycles light → dark → follow-the-system rather than showing three buttons: the footer is
 * one line, and this is the only preference a recipient can set here. It is a real button with
 * a changing label so a screen reader user hears the state, not just the icon.
 */
function ThemeSwitch({ prefs, onPrefs }: { prefs: Prefs; onPrefs: (next: Prefs) => void }) {
  const { t } = useI18n();
  const order: ThemePref[] = ["auto", "light", "dark"];
  const next = order[(order.indexOf(prefs.theme) + 1) % order.length] ?? "auto";
  return (
    <button
      type="button"
      className="serein-link serein-link--subtle"
      onClick={() => onPrefs({ ...prefs, theme: next })}
    >
      {t("theme.label")}: {t(`theme.${prefs.theme}`)}
    </button>
  );
}

export { LANGS };
