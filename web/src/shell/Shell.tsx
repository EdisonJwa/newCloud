// The shell.
//
// One 52px toolbar carrying three destinations, a search field, the inspector switch and the
// account menu. There is no sidebar: in a file manager a sidebar carries three jobs at once —
// where am I, what is the tree, and which section am I in — and once the breadcrumb and the
// listing answer the first two, it is left holding a single job while taking 216px from every
// screen.
//
// The three destinations are text, not a disclosure labelled with the place you are already
// in. That was the previous shell's single worst usability defect: a menu that says "Files"
// while you are looking at Files is discoverable to the person who built it and to nobody else.
//
// ## Where the other capabilities went
//
// The product has more than three sections. Trash, Activity, Access (app passwords and WebDAV)
// and Admin are real, tested capabilities, and they are places people visit to check or undo
// something rather than places they work. They live in Settings, reached from the account menu,
// so the primary bar stays a navigation aid instead of a feature inventory.

import { useEffect, useRef, useState, type ReactNode } from "react";
import { useI18n, LANGS, type Lang } from "../i18n";
import { Icon } from "../ui";
import type { Me } from "../types";
import type { Prefs } from "../appearance";
import { TEXT_SIZES, resolveContrast, resolveTheme } from "../appearance";
import type { Route, Screen, SettingsTab } from "../route";
import { serialize } from "../route";
import { isMac, shortcut } from "../platform";

export interface ShellProps {
  route: Route;
  me: Me | null;
  prefs: Prefs;
  onPrefs: (next: Prefs) => void;
  lang: Lang;
  onLang: (lang: Lang) => void;
  onNavigate: (screen: Screen) => void;
  onOpenSettings: (tab: SettingsTab) => void;
  onSearch: () => void;
  onToggleInspector: () => void;
  onSignOut: () => void;
  onUpload: () => void;
  onNewFolder: () => void;
  /** Shown under the toolbar when the browser is offline. */
  offline: boolean;
  onRetryOnline: () => void;
  /** The status strip's scope text, e.g. "All files" or "18 items · 5 folders". */
  scope: string;
  statusMeta: string;
  children: ReactNode;
}

const DESTINATIONS: readonly { screen: Screen; key: string; icon: string }[] = [
  { screen: "home", key: "nav.home", icon: "home" },
  { screen: "files", key: "nav.files", icon: "files" },
  { screen: "sharing", key: "nav.shared", icon: "shares" },
];

/** The inspector and viewer are not destinations; they are states of one. */
function activeDestination(route: Route): Screen {
  if (route.screen === "viewer") {
    return route.from.kind === "shared" ? "sharing" : "files";
  }
  return route.screen;
}

export function Shell(props: ShellProps) {
  const { t } = useI18n();
  const [account, setAccount] = useState(false);
  const [appearance, setAppearance] = useState(false);
  const active = activeDestination(props.route);
  const title =
    props.route.screen === "settings"
      ? t("settings.title")
      : props.route.screen === "sharing"
        ? t("nav.shared")
        : props.route.screen === "files"
          ? t("nav.files")
          : t("nav.home");

  return (
    // `wc-root` carries the fixed-chrome geometry vars (`--wc-topbar`, `--wc-tabbar`, …) the
    // sticky inspector, page fill and timeline day chips compute against, and `data-view`
    // marks the screens that replace the page — the viewer locks the scroll behind it. A
    // rename of either class is a silent layout break on every view, which is why both live
    // on the outermost element the shell renders.
    <div
      className="serein-shell wc-root-shell wc-root"
      data-view={props.route.screen === "viewer" ? "viewer" : undefined}
    >
      <a className="serein-skip-link" href="#main">
        {t("shell.skip")}
      </a>

      <header className="serein-topbar">
        <div className="serein-topbar__lead">
          <a
            className="serein-brand"
            href="/"
            aria-label={t("app.home")}
            onClick={(e) => {
              e.preventDefault();
              props.onNavigate("home");
            }}
          >
            <span className="serein-brand__mark" aria-hidden="true">
              <BrandGlyph />
            </span>
          </a>
          <nav className="serein-nav" aria-label={t("nav.sections")}>
            {DESTINATIONS.map((d) => (
              // Real anchors, not buttons: middle-click, ⌘/Ctrl-click and "copy link" are
              // how people keep two folders side by side. The click handler still routes
              // through the SPA so the sheet state survives.
              <a
                key={d.screen}
                className="serein-nav__item"
                href={serialize({ ...props.route, screen: d.screen, inspector: false })}
                aria-current={active === d.screen ? "page" : undefined}
                onClick={(e) => {
                  e.preventDefault();
                  props.onNavigate(d.screen);
                }}
              >
                <span className="serein-nav__icon">
                  <Icon name={d.icon} size={13} />
                </span>
                <span>{t(d.key)}</span>
              </a>
            ))}
          </nav>
        </div>

        <div className="serein-topbar__trail">
          <div className="serein-topbar__search">
            <button
              type="button"
              className="serein-search"
              onClick={props.onSearch}
              aria-label={t("search.open")}
            >
              <span className="serein-input-wrap serein-input-wrap--sm">
                <span className="serein-input-wrap__leading">
                  <Icon name="search" size={13} />
                </span>
                <span className="wc-search-facade">{t("search.placeholder")}</span>
                <span className="serein-input-wrap__trailing">
                  <kbd className="serein-search__kbd">{shortcut("K")}</kbd>
                </span>
              </span>
            </button>
          </div>

          <button
            type="button"
            className="serein-icon-btn"
            aria-pressed={props.route.inspector}
            aria-label={t("inspector.toggle")}
            title={t("inspector.toggle")}
            onClick={props.onToggleInspector}
          >
            <Icon name="sidebar" size={16} />
          </button>

          <button
            type="button"
            className="serein-icon-btn wc-accountbtn"
            aria-haspopup="menu"
            aria-expanded={account}
            onClick={() => setAccount((open) => !open)}
            title={props.me?.display_name || props.me?.email || t("account.open")}
          >
            <span className="serein-avatar" aria-hidden="true">
              {initialsOf(props.me)}
            </span>
            <span className="wc-live">{t("account.openWith", { name: displayOf(props.me) })}</span>
          </button>
        </div>
      </header>

      <header className="serein-mobile-top">
        <button
          type="button"
          className="serein-icon-btn"
          aria-label={t("search.open")}
          onClick={props.onSearch}
        >
          <Icon name="search" size={17} />
        </button>
        <span className="serein-mobile-top__title">{title}</span>
        <div className="serein-mobile-top__actions">
          <details className="serein-menu">
            {/*
              A native `<summary>` inside `<details>` is already a button to the accessibility
              tree. Adding `role="button"` on top replaces that semantics and drops the
              expanded state, so it is deliberately absent: `aria-label` plus the element's own
              role is the whole contract, and the framework's CSS supplies the icon-button look.
            */}
            <summary className="serein-icon-btn" aria-label={t("shell.create")}>
              <Icon name="plus" size={17} />
            </summary>
            <div className="serein-menu__surface serein-menu__surface--start" role="menu">
              <button
                type="button"
                className="serein-menu__item"
                role="menuitem"
                onClick={props.onUpload}
              >
                <span className="serein-menu__icon">
                  <Icon name="upload" size={13} />
                </span>
                {t("files.upload")}
              </button>
              <button
                type="button"
                className="serein-menu__item"
                role="menuitem"
                onClick={props.onNewFolder}
              >
                <span className="serein-menu__icon">
                  <Icon name="folderPlus" size={13} />
                </span>
                {t("files.newFolder")}
              </button>
              <div className="serein-menu__sep" role="separator" />
              <button
                type="button"
                className="serein-menu__item"
                role="menuitem"
                onClick={() => props.onOpenSettings("activity")}
              >
                <span className="serein-menu__icon">
                  <Icon name="pulse" size={13} />
                </span>
                {t("settings.activity")}
              </button>
              <button
                type="button"
                className="serein-menu__item"
                role="menuitem"
                onClick={() => props.onOpenSettings("trash")}
              >
                <span className="serein-menu__icon">
                  <Icon name="trash" size={13} />
                </span>
                {t("settings.trash")}
              </button>
            </div>
          </details>
          <button
            type="button"
            className="serein-icon-btn"
            aria-label={t("account.open")}
            aria-haspopup="menu"
            aria-expanded={account}
            onClick={() => setAccount((open) => !open)}
          >
            <span className="serein-avatar serein-avatar--sm" aria-hidden="true">
              {initialsOf(props.me)}
            </span>
          </button>
        </div>
      </header>

      {props.offline ? (
        <div className="wc-banner" role="status">
          <Icon name="offline" size={13} />
          <span>{t("state.offline")}</span>
          <span className="wc-grow" />
          <button type="button" className="serein-btn serein-btn--sm" onClick={props.onRetryOnline}>
            {t("state.retry")}
          </button>
        </div>
      ) : null}

      <main className="serein-shell__main" id="main" tabIndex={-1}>
        {props.children}
      </main>

      <footer className="serein-statusbar">
        <span className="serein-statusbar__scope">
          <Icon name="files" size={11} />
          {props.scope}
        </span>
        <span className="serein-statusbar__meta">
          {props.statusMeta}
          {isMac() ? "" : ` · ${shortcut("K")} ${t("search.short")}`}
        </span>
      </footer>

      <nav className="serein-tabbar" aria-label={t("nav.sections")}>
        {DESTINATIONS.map((d) => (
          <a
            key={d.screen}
            className="serein-tabbar__item"
            href={serialize({ ...props.route, screen: d.screen, inspector: false })}
            aria-current={active === d.screen ? "page" : undefined}
            onClick={(e) => {
              e.preventDefault();
              props.onNavigate(d.screen);
            }}
          >
            <Icon name={d.icon} size={19} />
            <span>{t(d.key)}</span>
          </a>
        ))}
      </nav>

      {account ? (
        <AccountMenu
          me={props.me}
          prefs={props.prefs}
          lang={props.lang}
          onLang={props.onLang}
          onClose={() => setAccount(false)}
          onOpenAppearance={() => {
            setAccount(false);
            setAppearance(true);
          }}
          onOpenSettings={(tab) => {
            setAccount(false);
            props.onOpenSettings(tab);
          }}
          onSignOut={props.onSignOut}
        />
      ) : null}

      {appearance ? (
        <AppearanceSheet
          prefs={props.prefs}
          onPrefs={props.onPrefs}
          lang={props.lang}
          onLang={props.onLang}
          onClose={() => setAppearance(false)}
        />
      ) : null}
    </div>
  );
}

function BrandGlyph() {
  // The mark: a stacked cloud/wave, drawn rather than loaded so it takes the token colours in
  // every appearance and needs no asset request.
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

/* ------------------------------------------------------------ account menu --- */

function AccountMenu({
  me,
  prefs,
  lang,
  onLang,
  onClose,
  onOpenAppearance,
  onOpenSettings,
  onSignOut,
}: {
  me: Me | null;
  prefs: Prefs;
  lang: Lang;
  onLang: (lang: Lang) => void;
  onClose: () => void;
  onOpenAppearance: () => void;
  onOpenSettings: (tab: SettingsTab) => void;
  onSignOut: () => void;
}) {
  const { t } = useI18n();
  const panel = useRef<HTMLDivElement>(null);
  const firstItem = useRef<HTMLButtonElement>(null);

  // The menu surface is the last thing in the DOM, so without this a keyboard user tabs
  // through the whole page before the menu they just opened.
  useEffect(() => {
    firstItem.current?.focus();
  }, []);

  // A menu that only closes on its own button strands a keyboard user. Escape and a click
  // outside are the two exits people expect, and focus returns to where it came from.
  useDismiss(panel, onClose);

  return (
    <>
      <div className="serein-scrim wc-scrim-clear" onClick={onClose} />
      <div
        ref={panel}
        className="serein-menu__surface wc-accountmenu"
        role="menu"
        aria-label={t("account.menu")}
      >
        <div className="wc-accountmenu__head">
          <span className="serein-avatar serein-avatar--lg" aria-hidden="true">
            {initialsOf(me)}
          </span>
          <div className="wc-grow">
            <div className="wc-accountmenu__name">{displayOf(me)}</div>
            <div className="wc-accountmenu__email">{me?.email ?? ""}</div>
          </div>
        </div>

        <div className="serein-menu__sep" role="separator" />
        <div className="serein-menu__label">{t("account.workspace")}</div>
        <button ref={firstItem} type="button" className="serein-menu__item" role="menuitem" onClick={onOpenAppearance}>
          <span className="serein-menu__icon">
            <Icon name="palette" size={13} />
          </span>
          {t("account.appearance")}
          <span className="serein-menu__shortcut">{appearanceSummary(t, prefs)}</span>
        </button>
        <button
          type="button"
          className="serein-menu__item"
          role="menuitem"
          onClick={() => onOpenSettings("trash")}
        >
          <span className="serein-menu__icon">
            <Icon name="trash" size={13} />
          </span>
          {t("settings.trash")}
        </button>
        <button
          type="button"
          className="serein-menu__item"
          role="menuitem"
          onClick={() => onOpenSettings("activity")}
        >
          <span className="serein-menu__icon">
            <Icon name="pulse" size={13} />
          </span>
          {t("settings.activity")}
        </button>
        <button
          type="button"
          className="serein-menu__item"
          role="menuitem"
          onClick={() => onOpenSettings("access")}
        >
          <span className="serein-menu__icon">
            <Icon name="key" size={13} />
          </span>
          {t("settings.access")}
        </button>
        <button
          type="button"
          className="serein-menu__item"
          role="menuitem"
          onClick={() => onOpenSettings("admin")}
        >
          <span className="serein-menu__icon">
            <Icon name="user" size={13} />
          </span>
          {t("settings.admin")}
        </button>

        <div className="serein-menu__sep" role="separator" />
        <div className="serein-menu__label">{t("account.language")}</div>
        {LANGS.map((l) => (
          <button
            key={l}
            type="button"
            className="serein-menu__item"
            role="menuitemradio"
            aria-checked={lang === l}
            onClick={() => onLang(l)}
          >
            <span className="serein-menu__icon">
              {lang === l ? <Icon name="check" size={13} /> : null}
            </span>
            {t(l === "en" ? "lang.en" : "lang.zh")}
          </button>
        ))}

        <div className="serein-menu__sep" role="separator" />
        <button
          type="button"
          className="serein-menu__item serein-menu__item--danger"
          role="menuitem"
          onClick={onSignOut}
        >
          <span className="serein-menu__icon">
            <Icon name="logout" size={13} />
          </span>
          {t("nav.signout")}
        </button>
      </div>
    </>
  );
}

/* --------------------------------------------------------- appearance sheet - */

/**
 * AppearanceSheet is a bottom sheet on a phone and a centred dialog otherwise; `<dialog>`
 * gives the top layer, focus containment and Escape for free, which is why it is a native
 * element rather than a positioned div.
 */
function AppearanceSheet({
  prefs,
  onPrefs,
  lang,
  onLang,
  onClose,
}: {
  prefs: Prefs;
  onPrefs: (next: Prefs) => void;
  lang: Lang;
  onLang: (lang: Lang) => void;
  onClose: () => void;
}) {
  const { t } = useI18n();
  const ref = useRef<HTMLDialogElement>(null);
  const opener = useRef<Element | null>(null);

  useEffect(() => {
    opener.current = document.activeElement;
    ref.current?.showModal();
    return () => {
      if (opener.current instanceof HTMLElement) opener.current.focus();
    };
  }, []);

  const theme = resolveTheme(prefs.theme);
  const contrast = resolveContrast(prefs.contrast);

  return (
    <dialog
      ref={ref}
      className="serein-sheet"
      aria-labelledby="appearance-title"
      onCancel={(e) => {
        e.preventDefault();
        onClose();
      }}
    >
      <div className="serein-sheet__grabber" aria-hidden="true" />
      <div className="serein-sheet__header">
        <h2 className="serein-sheet__title" id="appearance-title">
          {t("appearance.title")}
        </h2>
        <p className="serein-sheet__description">{t("appearance.description")}</p>
        <button
          type="button"
          className="serein-icon-btn serein-icon-btn--sm serein-sheet__close"
          aria-label={t("dlg.close")}
          onClick={onClose}
        >
          <Icon name="close" size={13} />
        </button>
      </div>

      <div className="serein-sheet__body">
        <div className="serein-field">
          <span className="serein-label" id="appearance-theme">
            {t("appearance.theme")}
          </span>
          <div className="serein-seg serein-seg--wide" role="group" aria-labelledby="appearance-theme">
            {(["auto", "light", "dark"] as const).map((value) => (
              <button
                key={value}
                type="button"
                className="serein-seg__btn"
                aria-pressed={prefs.theme === value}
                onClick={() => onPrefs({ ...prefs, theme: value })}
              >
                {t(`theme.${value}`)}
              </button>
            ))}
          </div>
          <span className="serein-hint">
            {prefs.theme === "auto" ? t("appearance.themeAuto", { resolved: t(`theme.${theme}`) }) : ""}
          </span>
        </div>

        <div className="serein-field">
          <span className="serein-label" id="appearance-contrast">
            {t("appearance.contrast")}
          </span>
          <div
            className="serein-seg serein-seg--wide"
            role="group"
            aria-labelledby="appearance-contrast"
          >
            {(["auto", "normal", "more"] as const).map((value) => (
              <button
                key={value}
                type="button"
                className="serein-seg__btn"
                aria-pressed={prefs.contrast === value}
                onClick={() => onPrefs({ ...prefs, contrast: value })}
              >
                {t(`contrast.${value}`)}
              </button>
            ))}
          </div>
          <span className="serein-hint">
            {prefs.contrast === "auto"
              ? t("appearance.contrastAuto", { resolved: t(`contrast.${contrast}`) })
              : ""}
          </span>
        </div>

        <label className="serein-switch wc-appearance-toggle">
          <input
            className="serein-switch__input"
            type="checkbox"
            role="switch"
            checked={prefs.textsize !== "normal"}
            onChange={(e) => onPrefs({ ...prefs, textsize: e.target.checked ? TEXT_SIZES[1]! : "normal" })}
          />
          <span className="serein-switch__track" aria-hidden="true" />
          <span className="serein-switch__text">
            {t("appearance.largeText")}
            <span className="serein-hint wc-block">{t("appearance.largeTextHint")}</span>
          </span>
        </label>

        <label className="serein-switch wc-appearance-toggle">
          <input
            className="serein-switch__input"
            type="checkbox"
            role="switch"
            checked={prefs.motion === "reduced"}
            onChange={(e) => onPrefs({ ...prefs, motion: e.target.checked ? "reduced" : "auto" })}
          />
          <span className="serein-switch__track" aria-hidden="true" />
          <span className="serein-switch__text">
            {t("appearance.reduceMotion")}
            <span className="serein-hint wc-block">{t("appearance.reduceMotionHint")}</span>
          </span>
        </label>

        <div className="serein-field">
          <span className="serein-label" id="appearance-language">
            {t("lang.label")}
          </span>
          <div
            className="serein-seg serein-seg--wide"
            role="group"
            aria-labelledby="appearance-language"
          >
            {LANGS.map((l) => (
              <button
                key={l}
                type="button"
                className="serein-seg__btn"
                aria-pressed={lang === l}
                onClick={() => onLang(l)}
              >
                {t(l === "en" ? "lang.en" : "lang.zh")}
              </button>
            ))}
          </div>
        </div>
      </div>

      <div className="serein-sheet__footer">
        <button type="button" className="serein-btn" onClick={onClose}>
          {t("dlg.done")}
        </button>
      </div>
    </dialog>
  );
}

/* ---------------------------------------------------------------- helpers --- */

export function initialsOf(me: Me | null): string {
  const source = me?.display_name || me?.username || me?.email || "";
  if (!source) return "?";
  const parts = source.replace(/@.*$/, "").split(/[\s._-]+/).filter(Boolean);
  if (parts.length === 0) return source.slice(0, 2).toUpperCase();
  if (parts.length === 1) return (parts[0] ?? "").slice(0, 2).toUpperCase();
  return `${(parts[0] ?? "")[0] ?? ""}${(parts[1] ?? "")[0] ?? ""}`.toUpperCase();
}

export function displayOf(me: Me | null): string {
  return me?.display_name || me?.username || me?.email || "";
}

function appearanceSummary(
  t: (key: string, params?: Record<string, string | number>) => string,
  prefs: Prefs,
): string {
  const theme = prefs.theme === "auto" ? t("theme.auto") : t(`theme.${prefs.theme}`);
  return prefs.textsize === "normal" ? theme : `${theme} · ${t("appearance.largeText")}`;
}

/**
 * useDismiss closes an overlay on Escape or a click outside it.
 *
 * Serein's `details`-based menus close themselves natively. The account menu is a portal-style
 * panel positioned against the top bar instead, because it outgrew a `details` surface's
 * single-column width, so it needs the two exits those menus get for free.
 */
export function useDismiss(ref: React.RefObject<HTMLElement | null>, onClose: () => void): void {
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        onClose();
      }
    };
    const onDown = (e: MouseEvent) => {
      const node = ref.current;
      if (node && e.target instanceof Node && !node.contains(e.target)) onClose();
    };
    window.addEventListener("keydown", onKey);
    // `capture` so a control that stops propagation cannot trap a click that was meant to close.
    window.addEventListener("mousedown", onDown, true);
    return () => {
      window.removeEventListener("keydown", onKey);
      window.removeEventListener("mousedown", onDown, true);
    };
  }, [ref, onClose]);
}
