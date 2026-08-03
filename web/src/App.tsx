// The shell.
//
// Navigation is a horizontal set of sections in the top bar. There is no persistent sidebar:
// in a file manager the sidebar carries three jobs at once — where am I, what is the tree, and
// which section am I in — and once the column view answers the first two, the sidebar is left
// holding a single job while taking 216px from every screen.

import { useCallback, useEffect, useMemo, useState } from "react";
import { api, APIError, setOnline } from "./api";
import { createI18n, detectLang, I18nContext, LANGS, type Lang } from "./i18n";
import { Icon, ToastProvider, useToast } from "./ui";
import { Palette, type Command } from "./components/Palette";
import { Home } from "./views/Home";
import { Files } from "./views/Files";
import { Access, Shared, Trash } from "./views/Simple";
import { Activity } from "./views/Activity";
import { ShareLanding } from "./views/ShareLanding";
import type { Me } from "./types";

type Section = "home" | "files" | "shared" | "activity" | "trash" | "access";

const SECTIONS: Section[] = ["home", "files", "shared", "activity", "trash", "access"];
const ICONS: Record<Section, string> = {
  home: "home",
  files: "files",
  shared: "shares",
  activity: "pulse",
  trash: "trash",
  access: "key",
};

type Theme = "auto" | "light" | "dark" | "hc";

// Languages that read right-to-left. None ship yet; the list exists so adding one is a
// message table plus an entry here, not a second stylesheet.
const RTL_LANGS: readonly string[] = ["ar", "he", "fa", "ur"];

function stored(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return null;
  }
}

function store(key: string, value: string): void {
  try {
    localStorage.setItem(key, value);
  } catch {
    /* private mode: the preference simply does not persist */
  }
}

/** resolveTheme turns the preference into the attribute the stylesheet switches on. */
function resolveTheme(pref: Theme): "light" | "dark" | "hc" {
  if (pref !== "auto") return pref;
  const contrast = window.matchMedia("(prefers-contrast: more)");
  if (contrast.matches) return "hc";
  return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

export function App() {
  return (
    <ToastProvider>
      {location.pathname.startsWith("/s/") ? (
        <ShareLanding token={location.pathname.slice(3).replace(/\/$/, "")} />
      ) : (
        <Shell />
      )}
    </ToastProvider>
  );
}

function Shell() {
  const [lang, setLang] = useState<Lang>(detectLang);
  const i18n = useMemo(() => createI18n(lang), [lang]);
  const [theme, setTheme] = useState<Theme>(() => (stored("wc.theme") as Theme) || "auto");
  const [section, setSection] = useState<Section>(() => {
    const fromURL = new URLSearchParams(location.search).get("view");
    return (SECTIONS as string[]).includes(fromURL ?? "") ? (fromURL as Section) : "home";
  });
  const [filePath, setFilePath] = useState(() => new URLSearchParams(location.search).get("path") ?? "");
  const [me, setMe] = useState<Me | null>(null);
  const [offline, setOffline] = useState(!navigator.onLine);
  const [palette, setPalette] = useState(false);
  const [reloadKey] = useState(0);
  const toast = useToast();

  const { t } = i18n;

  /* ---------- session ---------- */

  useEffect(() => {
    api
      .me()
      .then(setMe)
      .catch((err) => {
        // An unauthenticated session is not an error state to render: send them to the IdP.
        if (err instanceof APIError && err.status === 401) location.href = "/api/auth/login";
      });
  }, []);

  /* ---------- theme ---------- */

  useEffect(() => {
    const apply = () => {
      document.documentElement.dataset.theme = resolveTheme(theme);
    };
    apply();
    store("wc.theme", theme);
    const media = window.matchMedia("(prefers-color-scheme: dark)");
    media.addEventListener("change", apply);
    return () => media.removeEventListener("change", apply);
  }, [theme]);

  /* ---------- language and direction ---------- */

  useEffect(() => {
    document.documentElement.lang = lang;
    // No RTL language ships yet, but the direction attribute and the logical properties are
    // already in place, so adding one is a message table plus this switch.
    document.documentElement.dir = RTL_LANGS.includes(lang) ? "rtl" : "ltr";
    store("wc.lang", lang);
  }, [lang]);

  /* ---------- connectivity ---------- */

  useEffect(() => {
    const update = () => {
      setOffline(!navigator.onLine);
      setOnline(navigator.onLine);
    };
    window.addEventListener("online", update);
    window.addEventListener("offline", update);
    return () => {
      window.removeEventListener("online", update);
      window.removeEventListener("offline", update);
    };
  }, []);

  /* ---------- routing ---------- */

  const navigate = useCallback((next: Section, path?: string) => {
    setSection(next);
    if (path !== undefined) setFilePath(path);
    const params = new URLSearchParams();
    if (next !== "home") params.set("view", next);
    if (next === "files" && path) params.set("path", path);
    const query = params.toString();
    history.replaceState(null, "", query ? `/?${query}` : "/");
  }, []);

  const openPath = useCallback(
    (path: string) => {
      const clean = path.replace(/^\//, "");
      navigate("files", clean.includes("/") ? clean.slice(0, clean.lastIndexOf("/")) : "");
    },
    [navigate],
  );

  /* ---------- palette ---------- */

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const typing =
        e.target instanceof HTMLElement &&
        (e.target.tagName === "INPUT" || e.target.tagName === "TEXTAREA" || e.target.isContentEditable);
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setPalette((open) => !open);
      } else if (e.key === "/" && !typing) {
        e.preventDefault();
        setPalette(true);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  const commands = useMemo<Command[]>(() => {
    const list: Command[] = SECTIONS.map((s) => ({
      id: `go-${s}`,
      label: t(`palette.go${s[0]?.toUpperCase() ?? ""}${s.slice(1)}`),
      hint: t(`nav.${s}`),
      run: () => navigate(s),
    }));

    list.push({
      id: "theme",
      label: t("palette.theme"),
      hint: t("theme.label"),
      run: () => {
        const order: Theme[] = ["auto", "light", "dark", "hc"];
        setTheme(order[(order.indexOf(theme) + 1) % order.length] ?? "auto");
      },
    });
    list.push({
      id: "lang",
      label: t("palette.lang"),
      hint: lang,
      run: () => setLang(LANGS[(LANGS.indexOf(lang) + 1) % LANGS.length] ?? "en"),
    });
    return list;
  }, [navigate, t, theme, lang]);

  return (
    <I18nContext.Provider value={i18n}>
      <div className="app">
        <header className="topbar">
          <span className="brand">
            <span className="mark" aria-hidden="true" />
            <span className="word">{t("app.name")}</span>
          </span>

          <nav className="nav" aria-label={t("nav.sections")}>
            {SECTIONS.map((s) => (
              <button
                key={s}
                type="button"
                aria-current={section === s ? "page" : undefined}
                onClick={() => navigate(s)}
                title={t(`nav.${s}`)}
              >
                <Icon name={ICONS[s]} size={13} />
                <span className="word">{t(`nav.${s}`)}</span>
              </button>
            ))}
          </nav>

          <span className="spacer" />

          <div className="searchbox">
            <span className="icon">
              <Icon name="search" size={13} />
            </span>
            <input
              readOnly
              onFocus={() => setPalette(true)}
              placeholder={t("palette.placeholder")}
              aria-label={t("palette.placeholder")}
            />
          </div>

          <select
            className="pick"
            value={theme}
            onChange={(e) => setTheme(e.target.value as Theme)}
            aria-label={t("theme.label")}
            title={t("theme.label")}
          >
            <option value="auto">{t("theme.auto")}</option>
            <option value="light">{t("theme.light")}</option>
            <option value="dark">{t("theme.dark")}</option>
            <option value="hc">{t("theme.hc")}</option>
          </select>

          <button
            type="button"
            className="icon-btn"
            onClick={() => setLang(lang === "en" ? "zh-Hans" : "en")}
            aria-label={t("lang.label")}
            title={`${t("lang.label")}: ${lang}`}
          >
            <span className="mono langmark">{lang === "en" ? "EN" : "中"}</span>
          </button>

          <button
            type="button"
            className="icon-btn"
            onClick={() => {
              void api.logout().then(() => {
                location.href = "/";
              });
            }}
            aria-label={t("nav.signout")}
            title={me?.email ?? t("nav.signout")}
          >
            <Icon name="user" size={14} />
          </button>
        </header>

        {offline ? (
          <div className="banner" role="status">
            <Icon name="offline" size={13} />
            <span>{t("state.offline")}</span>
            <span className="spacer" />
            <button type="button" className="btn sm" onClick={() => toast.info(t("state.loading"))}>
              {t("state.retry")}
            </button>
          </div>
        ) : null}

        <main className="main">
          {section === "home" ? (
            <Home
              me={me}
              reloadKey={reloadKey}
              onOpenFiles={(path) => navigate("files", path)}
              onOpenShared={() => navigate("shared")}
            />
          ) : null}
          {section === "files" ? (
            <Files initialPath={filePath} reloadKey={reloadKey} onPathChange={setFilePath} />
          ) : null}
          {section === "shared" ? <Shared reloadKey={reloadKey} /> : null}
          {section === "activity" ? <Activity reloadKey={reloadKey} /> : null}
          {section === "trash" ? <Trash reloadKey={reloadKey} /> : null}
          {section === "access" ? <Access reloadKey={reloadKey} /> : null}
        </main>
      </div>

      {palette ? <Palette commands={commands} onClose={() => setPalette(false)} onOpenPath={openPath} /> : null}
    </I18nContext.Provider>
  );
}


