// The application controller.
//
// One component owns the things that must not be duplicated: the current route, the session,
// the appearance preferences, the upload trigger and the top-level overlays (search, viewer).
// Views are pure functions of the route and receive one `onRoute` callback; none of them owns
// history. That is what makes the URL the single source of truth rather than a side effect.
//
// There are two roots because there are two audiences. `/s/{token}` is an unauthenticated
// recipient page: it has no session, no account menu and no shell, and it must not wait for
// `/api/auth/me` (which answers 401 for a recipient and would otherwise bounce them to the
// identity provider's login). Everything else is the signed-in application.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { api, APIError, setOnline, sharedContentURL } from "./api";
import { createI18n, detectLang, I18nContext, useI18n, type Lang } from "./i18n";
import { AnnounceProvider, ToastProvider, useToast } from "./ui";
import { Shell } from "./shell/Shell";
import { SearchPanel, type Command } from "./shell/SearchPanel";
import { Home, recordOpened } from "./views/Home";
import { Files } from "./views/Files";
import { Sharing } from "./views/Sharing";
import { Settings } from "./views/Settings";
import { Viewer, useCollection, sharedThumbnailURL } from "./views/Viewer";
import { ShareLanding } from "./views/ShareLanding";
import { applyPrefs, readPrefs, savePrefs, type Prefs } from "./appearance";
import { Router, DEFAULT_ROUTE, type Route, type Screen, type SettingsTab } from "./route";
import { shortcut } from "./platform";
import { parentOf } from "./nodes";
import type { FileNode, Me, SharedNode } from "./types";

/** The router is module-scoped: it must survive a React re-render and own the popstate
 *  listener exactly once. */
const router = new Router();

export function App() {
  return (
    <ToastProvider>
      <AnnounceProvider>
        <Root />
      </AnnounceProvider>
    </ToastProvider>
  );
}

function Root() {
  const [prefs, setPrefs] = useState<Prefs>(() => readPrefs());
  const [lang, setLang] = useState<Lang>(detectLang);
  const i18n = useMemo(() => createI18n(lang), [lang]);

  // The appearance attributes have to be on the element the token sheet is scoped to before the
  // first meaningful paint. The stylesheet's own default already handles "follow the system",
  // which is the common case; this applies an explicit choice.
  useEffect(() => {
    applyPrefs(prefs);
  }, [prefs]);

  useEffect(() => {
    document.documentElement.lang = lang;
    try {
      localStorage.setItem("wc.lang", lang);
    } catch {
      /* not persisted; the page still renders in the chosen language */
    }
  }, [lang]);

  const updatePrefs = useCallback((next: Prefs) => {
    setPrefs(next);
    savePrefs(next);
    applyPrefs(next);
  }, []);

  const token = shareToken(location.pathname);

  return (
    <I18nContext.Provider value={i18n}>
      {token ? (
        <ShareLanding token={token} lang={lang} onLang={setLang} prefs={prefs} onPrefs={updatePrefs} />
      ) : (
        <Application
          prefs={prefs}
          onPrefs={updatePrefs}
          lang={lang}
          onLang={setLang}
        />
      )}
    </I18nContext.Provider>
  );
}

/**
 * shareToken reads `/s/{token}` out of the path.
 *
 * A separate path route rather than a query parameter because the recipient page is a
 * different application: it has no session, its own server protocol and its own appearance
 * controls. Keeping it in the query would put an unauthenticated surface inside the shell's
 * route switch.
 */
function shareToken(pathname: string): string | null {
  const match = /^\/s\/([^/]+)\/?$/.exec(pathname);
  return match?.[1] ? decodeURIComponent(match[1]) : null;
}

/* ------------------------------------------------------------- application --- */

function Application({
  prefs,
  onPrefs,
  lang,
  onLang,
}: {
  prefs: Prefs;
  onPrefs: (next: Prefs) => void;
  lang: Lang;
  onLang: (lang: Lang) => void;
}) {
  const { t } = useI18n();
  const toast = useToast();
  const [route, setRoute] = useState<Route>(() => router.route);
  const [me, setMe] = useState<Me | null>(null);
  const [offline, setOffline] = useState(!navigator.onLine);
  const [search, setSearch] = useState(false);
  const [reloadKey, setReloadKey] = useState(0);
  const [pendingAction, setPendingAction] = useState<"upload" | "mkdir" | null>(null);
  // The received-share preview is carried in memory rather than resolved from the URL: a
  // shared node's path lives in the owner's namespace, so a path in the URL would resolve
  // inside the caller's own tree and find nothing — or somebody else's file of the same name.
  const sharedPreview = useRef<SharedNode | null>(null);

  const collection = useCollection(route, route.screen === "viewer");

  /* ---------- session ---------- */

  useEffect(() => {
    api
      .me()
      .then(setMe)
      .catch((err) => {
        // An unauthenticated session is not an error state to render: send them to the IdP.
        if (err instanceof APIError && err.status === 401) location.href = "/api/auth/login";
        else setMe(null);
      });
  }, []);

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

  useEffect(() => router.subscribe((next) => setRoute(next)), []);

  const go = useCallback((changes: Partial<Route>, mode: "push" | "replace") => {
    const next = { ...router.route, ...changes };
    if (mode === "push") router.navigate(next);
    else router.update(next);
  }, []);

  const navigate = useCallback(
    (screen: Screen) => {
      // Entering a destination from the bar is a fresh start for its transient state, but the
      // Files location is remembered so switching to Sharing and back does not lose the folder
      // you were in.
      go({ screen, inspector: false, share: "" }, "push");
    },
    [go],
  );

  const openSettings = useCallback(
    (tab: SettingsTab) => {
      go({ screen: "settings", settingsTab: tab, inspector: false }, "push");
    },
    [go],
  );

  // Escape closes the viewer; browser Back closes it too, because the viewer is a history
  // entry. Both land here.
  const closeViewer = useCallback(() => {
    const from = router.route.from;
    if (from.kind === "folder") {
      go({ screen: "files", space: from.path, at: 0 }, "push");
      return;
    }
    if (from.kind === "search") {
      go({ screen: "files", q: from.q, at: 0 }, "push");
      return;
    }
    if (from.kind === "shared") {
      go({ screen: "sharing", tab: "received", at: 0 }, "push");
      return;
    }
    if (from.kind === "single") {
      go({ screen: "files", space: parentOf(from.path), at: 0 }, "push");
      return;
    }
    go({ screen: "home", at: 0 }, "push");
  }, [go]);

  const openFile = useCallback(
    (entry: FileNode, from: Route["from"]) => {
      recordOpened(entry);
      sharedPreview.current = null;
      go({ screen: "viewer", file: entry.path, from, at: 0, inspector: false }, "push");
    },
    [go],
  );

  const openReceived = useCallback(
    (node: SharedNode) => {
      if (node.kind === "dir") {
        // A received folder is browsed in the Sharing view's tree rather than the Files
        // listing, because the Files listing addresses nodes by path inside the caller's tree.
        toast.info(t("received.browseHint", { name: node.name }));
        return;
      }
      sharedPreview.current = node;
      go({ screen: "viewer", file: node.id, from: { kind: "shared" }, at: 0, inspector: false }, "push");
    },
    [go, toast, t],
  );

  /*
   * The viewer's index, resolved from the file the URL names.
   *
   * `file` is the authority and `at` is a convenience. They disagree in two ordinary cases:
   *
   *   * a cold load of a link that names a file but no index (every share, every hand-written
   *     URL, and the previous version of this component, which silently showed the collection's
   *     first file instead of the one in the URL);
   *   * a file that has moved within its collection since the URL was written.
   *
   * So the index is derived from the file whenever the two disagree, and `at` only wins when it
   * actually points at the named file. This is the whole "a link opens what I was looking at"
   * contract, so it is decided in one place.
   */
  const viewerIndex = useMemo(() => {
    if (route.screen !== "viewer" || !collection.entries) return route.at;
    if (collection.entries[route.at]?.path === route.file) return route.at;
    const found = collection.entries.findIndex((f) => f.path === route.file);
    return found >= 0 ? found : 0;
  }, [route.screen, route.at, route.file, collection.entries]);

  const viewerRoute: Route = useMemo(
    () => (route.screen === "viewer" && viewerIndex !== route.at ? { ...route, at: viewerIndex } : route),
    [route, viewerIndex],
  );

  /* ---------- search ---------- */

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const target = e.target as HTMLElement | null;
      const typing =
        target && (target.closest("input, textarea, select") || target.isContentEditable);
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setSearch((open) => !open);
      } else if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "i") {
        e.preventDefault();
        go({ inspector: !router.route.inspector }, "push");
      } else if (e.key === "/" && !typing) {
        e.preventDefault();
        setSearch(true);
      } else if (e.key === "?" && !typing) {
        e.preventDefault();
        go({ inspector: true, pane: "info" }, "push");
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [go]);

  const commands = useMemo<Command[]>(() => {
    const list: Command[] = [
      {
        id: "home",
        label: t("search.goHome"),
        hint: t("nav.home"),
        icon: "home",
        run: () => navigate("home"),
      },
      {
        id: "files",
        label: t("search.goFiles"),
        hint: t("nav.files"),
        icon: "files",
        run: () => navigate("files"),
      },
      {
        id: "sharing-received",
        label: t("search.goReceived"),
        hint: t("nav.shared"),
        icon: "inbox",
        run: () => go({ screen: "sharing", tab: "received" }, "push"),
      },
      {
        id: "sharing-managed",
        label: t("search.goManaged"),
        hint: t("nav.shared"),
        icon: "link",
        run: () => go({ screen: "sharing", tab: "managed" }, "push"),
      },
      {
        id: "trash",
        label: t("search.goTrash"),
        hint: t("settings.title"),
        icon: "trash",
        run: () => openSettings("trash"),
      },
      {
        id: "activity",
        label: t("search.goActivity"),
        hint: t("settings.title"),
        icon: "pulse",
        run: () => openSettings("activity"),
      },
      {
        id: "access",
        label: t("search.goAccess"),
        hint: t("settings.title"),
        icon: "key",
        run: () => openSettings("access"),
      },
      {
        id: "upload",
        label: t("search.uploadNow"),
        hint: shortcut("U"),
        icon: "upload",
        run: () => {
          if (router.route.screen !== "files") go({ screen: "files" }, "push");
          setPendingAction("upload");
        },
      },
      {
        id: "new-folder",
        label: t("search.newFolderNow"),
        hint: "",
        icon: "folderPlus",
        run: () => {
          if (router.route.screen !== "files") go({ screen: "files" }, "push");
          setPendingAction("mkdir");
        },
      },
      {
        id: "theme",
        label: t("theme.label"),
        hint: t(`theme.${prefs.theme}`),
        icon: "palette",
        run: () => {
          const order = ["auto", "light", "dark"] as const;
          const next = order[(order.indexOf(prefs.theme as (typeof order)[number]) + 1) % order.length] ?? "auto";
          onPrefs({ ...prefs, theme: next });
        },
      },
      {
        id: "large-text",
        label: t("appearance.largeText"),
        hint: prefs.textsize === "normal" ? t("appearance.off") : t("appearance.on"),
        icon: "settings",
        run: () => onPrefs({ ...prefs, textsize: prefs.textsize === "normal" ? "large" : "normal" }),
      },
    ];
    return list;
  }, [navigate, go, openSettings, prefs, onPrefs, t]);

  /* ---------- the shell's status line ---------- */

  const status = useMemo(() => {
    if (route.screen === "viewer") {
      return {
        scope: collection.label,
        meta: `${Math.min(route.at + 1, Math.max(1, collection.entries?.length ?? 1))} / ${collection.entries?.length ?? 0}`,
      };
    }
    if (route.screen === "files") {
      return {
        scope: route.space || t("files.root"),
        meta: t("files.statusMeta"),
      };
    }
    if (route.screen === "sharing") {
      return {
        scope: t(route.tab === "received" ? "shared.tabReceived" : "shared.tabLinks"),
        meta: t("shared.statusMeta"),
      };
    }
    if (route.screen === "settings") {
      return { scope: t("settings.title"), meta: t(`settings.trash`) };
    }    return { scope: t("nav.home"), meta: t("app.name") };
  }, [route.screen, route.space, route.tab, route.at, collection.label, collection.entries, t]);

  /* ---------- render ---------- */

  return (
    <Shell
      route={route}
      me={me}
      prefs={prefs}
      onPrefs={onPrefs}
      lang={lang}
      onLang={onLang}
      onNavigate={navigate}
      onOpenSettings={openSettings}
      onSearch={() => setSearch(true)}
      onToggleInspector={() => go({ inspector: !route.inspector }, "push")}
      onSignOut={() => {
        void api.logout().then(() => {
          location.href = "/";
        });
      }}
      onUpload={() => {
        if (route.screen !== "files") go({ screen: "files" }, "push");
        setPendingAction("upload");
      }}
      onNewFolder={() => {
        if (route.screen !== "files") go({ screen: "files" }, "push");
        setPendingAction("mkdir");
      }}
      offline={offline}
      onRetryOnline={() => {
        setReloadKey((k) => k + 1);
        toast.info(t("state.retrying"));
      }}
      scope={status.scope}
      statusMeta={status.meta}
    >
      {route.screen === "home" ? (
        <Home
          me={me}
          reloadKey={reloadKey}
          onRoute={go}
          onOpenFile={openFile}
          onOpenReceived={openReceived}
          onUpload={() => {
            go({ screen: "files" }, "push");
            setPendingAction("upload");
          }}
          pendingAction={pendingAction}
          onActionHandled={() => setPendingAction(null)}
        />
      ) : null}

      {route.screen === "files" ? (
        <Files
          route={route}
          reloadKey={reloadKey}
          onRoute={go}
          onOpenFile={openFile}
          me={me}
          pendingAction={pendingAction}
          onActionHandled={() => setPendingAction(null)}
        />
      ) : null}

      {route.screen === "sharing" ? (
        <Sharing
          route={route}
          reloadKey={reloadKey}
          onRoute={go}
          onOpenReceived={openReceived}
          onOpenSharedFile={openReceived}
          onPreviewLink={(share) => {
            // A preview needs the share's token, which the list deliberately does not carry
            // (only its hash is stored). Opening the recipient page therefore means issuing
            // the link again, which the interface says rather than silently failing.
            if (share.url) window.open(share.url, "_blank", "noopener,noreferrer");
            else toast.warning(t("shared.noURLAvailable"));
          }}
        />
      ) : null}

      {route.screen === "settings" ? (
        <Settings route={route} me={me} reloadKey={reloadKey} onRoute={go} />
      ) : null}

      {route.screen === "viewer" ? (
        <Viewer
          route={viewerRoute}
          collection={collection}
          onIndex={(index) => {
            // Stepping replaces the entry rather than pushing one: forty presses of Next should
            // not be forty presses of Back to leave the viewer. Both `file` and `at` move
            // together so a reload (or a copied URL) resumes on the file that is on screen.
            const next = collection.entries?.[index];
            go({ at: index, ...(next ? { file: next.path } : {}) }, "replace");
          }}
          onClose={closeViewer}
          onRetry={() => setReloadKey((k) => k + 1)}
          onToggleInspector={() => go({ inspector: !route.inspector }, "push")}
          inspectorOpen={route.inspector}
        />
      ) : null}

      {search ? (
        <SearchPanel
          commands={commands}
          onClose={() => setSearch(false)}
          onOpenFile={(entry) => openFile(entry, { kind: "search", q: "" })}
          onOpenFolder={(path) => go({ screen: "files", space: path, q: "", kind: "all" }, "push")}
        />
      ) : null}
    </Shell>
  );
}

export { sharedThumbnailURL, sharedContentURL, DEFAULT_ROUTE };
