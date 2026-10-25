// The route.
//
// Every view the product has is addressable, so a link someone pastes into chat opens what they
// were looking at, reload keeps it, and Back leaves the viewer instead of the application. That
// is not a nicety in a file manager: "the file I shared" is usually communicated as a URL.
//
// The scheme is query-based, matching the approved prototype (`?screen=files&space=…`) rather
// than path segments. Two reasons: the SPA is served from one HTML file by a catch-all
// handler, so a path route would need the server to know the application's information
// architecture, and `/s/{token}` genuinely must stay a separate path because it is an
// unauthenticated recipient page with its own protocol.
//
// ## The two histories
//
// `push` creates a history entry and is for navigation: changing section, entering a folder,
// opening the viewer, switching the sharing tab, committing a search. `replace` updates the
// current entry and is for state the person did not navigate to: selection, sort, layout,
// an open inspector. Getting this backwards is what produces a Back button that needs four
// presses to leave a page, or one that skips the file you just closed.
//
// ## Authority
//
// The URL is the authority for everything it carries. A cold load and a `popstate` go through
// the same `parse`, so a hand-edited or truncated URL lands somewhere valid rather than
// half-restored: an unknown screen is Home, an unknown folder is All files, an unknown viewer
// collection is the file's own folder.

/** Which top-level destination is showing. Settings sub-routes are one destination with tabs. */
export type Screen = "home" | "files" | "sharing" | "settings" | "viewer";

/** The Files location. `all` is the aggregation; `root` is the workspace root directory. */
export type Space = string; // "all" | "root" | a folder path ("" means root)

export type ShareTab = "received" | "managed";

export type SettingsTab = "trash" | "activity" | "access" | "admin";

/** Which viewer collection a file was opened from. Also the identity used in the URL. */
export type ViewerFrom =
  | { kind: "folder"; path: string }
  | { kind: "recent" }
  | { kind: "search"; q: string }
  | { kind: "shared" }
  | { kind: "single"; path: string };

// The inspector's panes. `comments` is in the URL like the rest, so a link to a file's discussion is
// a link — the pane survives a reload and can be shared (core-scope §6).
export type Pane = "info" | "versions" | "comments" | "activity" | "share";

export type SortKey = "name" | "size" | "modified";
export type SortDir = "asc" | "desc";
export type ViewMode = "rows" | "grid";

/** The kind filter on the Files listing. */
export type KindFilter = "all" | "folders" | "documents" | "images" | "media" | "other";

export interface Route {
  screen: Screen;
  /** Files location. */
  space: Space;
  /** Viewer file path, relative to the workspace. */
  file: string;
  from: ViewerFrom;
  /** Viewer collection index, so a reload restores the file you were looking at. */
  at: number;
  tab: ShareTab;
  /** Selected outbound share id, for the link inspector. */
  share: string;
  settingsTab: SettingsTab;
  pane: Pane;
  /** Committed search, as opposed to the local filter. */
  q: string;
  kind: KindFilter;
  /**
   * tag narrows the listing to one label (core-scope §5).
   *
   * It is in the URL, not in component state, for the same reason `q` is: a filtered view is a place
   * a user can come back to, and losing it on reload makes the back button a lie.
   */
  tag: string;
  /** starred restricts the listing to the caller's favorites (core-scope §4). */
  starred: boolean;
  sort: SortKey;
  dir: SortDir;
  view: ViewMode;
  /** Inspector open. `replace`, and it is a reviewable state rather than session-only. */
  inspector: boolean;
}

export const DEFAULT_ROUTE: Route = {
  screen: "home",
  space: "",
  file: "",
  from: { kind: "recent" },
  at: 0,
  tab: "received",
  share: "",
  settingsTab: "trash",
  pane: "info",
  q: "",
  kind: "all",
  tag: "",
  starred: false,
  sort: "name",
  dir: "asc",
  view: "rows",
  inspector: false,
};

const SCREENS: readonly Screen[] = ["home", "files", "sharing", "settings", "viewer"];
const SHARE_TABS: readonly ShareTab[] = ["received", "managed"];
const SETTINGS_TABS: readonly SettingsTab[] = ["trash", "activity", "access", "admin"];
const PANES: readonly Pane[] = ["info", "versions", "comments", "activity", "share"];
const SORTS: readonly SortKey[] = ["name", "size", "modified"];
const KINDS: readonly KindFilter[] = ["all", "folders", "documents", "images", "media", "other"];

function oneOf<T extends string>(value: string | null, allowed: readonly T[], fallback: T): T {
  return (allowed as readonly string[]).includes(value ?? "") ? (value as T) : fallback;
}

/**
 * parseFrom reads the `from` parameter.
 *
 * The collection identity has to survive a reload, and it has to be storable in one string
 * because a query parameter cannot hold a discriminated union. The encodings are the
 * prototype's, unchanged: `folder:<path>`, `recent`, `search:<q>`, `shared`, `single:<path>`.
 */
export function parseFrom(raw: string | null): ViewerFrom {
  if (!raw) return { kind: "recent" };
  if (raw === "recent") return { kind: "recent" };
  if (raw === "shared") return { kind: "shared" };
  if (raw.startsWith("folder:")) return { kind: "folder", path: raw.slice(7) };
  if (raw.startsWith("search:")) return { kind: "search", q: raw.slice(7) };
  if (raw.startsWith("single:")) return { kind: "single", path: raw.slice(7) };
  // An unknown collection is not a reason to refuse the file: the viewer falls back to the
  // file's own folder, which is what a person who lost a query string most likely wanted.
  return { kind: "recent" };
}

export function formatFrom(from: ViewerFrom): string {
  switch (from.kind) {
    case "folder":
      return `folder:${from.path}`;
    case "search":
      return `search:${from.q}`;
    case "shared":
      return "shared";
    case "single":
      return `single:${from.path}`;
    case "recent":
      return "recent";
  }
}

/**
 * parse turns a URLSearchParams into a complete route.
 *
 * Every field has a defined fallback, so this function never throws and never returns a
 * partially-built route. That is what makes a cold load and a `popstate` interchangeable.
 */
export function parse(params: URLSearchParams): Route {
  const screen = oneOf(params.get("screen"), SCREENS, "home");

  const tab = oneOf(params.get("tab"), SHARE_TABS, "received");
  const settingsTab = oneOf(params.get("settings"), SETTINGS_TABS, "trash");
  const pane = oneOf(params.get("pane"), PANES, "info");
  const sort = oneOf(params.get("sort"), SORTS, "name");
  const kind = oneOf(params.get("kind"), KINDS, "all");

  const atRaw = Number.parseInt(params.get("at") ?? "", 10);

  return {
    screen,
    space: params.get("space") ?? "",
    file: params.get("file") ?? "",
    from: parseFrom(params.get("from")),
    at: Number.isFinite(atRaw) && atRaw >= 0 ? atRaw : 0,
    tab,
    share: params.get("share") ?? "",
    settingsTab,
    pane,
    q: params.get("q") ?? "",
    kind,
    tag: params.get("tag") ?? "",
    starred: params.get("starred") === "1",
    sort,
    dir: params.get("dir") === "desc" ? "desc" : "asc",
    view: params.get("view") === "grid" ? "grid" : "rows",
    inspector: params.get("inspector") === "1",
  };
}

/**
 * serialize writes only what differs from a default.
 *
 * A URL that restates every default is unreadable and, worse, gives the browser two different
 * URLs for the same view, which breaks "does this link open what I am looking at?".
 */
export function serialize(route: Route): string {
  const params = new URLSearchParams();

  if (route.screen !== "home") params.set("screen", route.screen);

  if (route.screen === "files") {
    if (route.space) params.set("space", route.space);
    if (route.kind !== "all") params.set("kind", route.kind);
    if (route.sort !== "name") params.set("sort", route.sort);
    if (route.dir !== "asc") params.set("dir", route.dir);
    if (route.view !== "rows") params.set("view", route.view);
    if (route.q) params.set("q", route.q);
    if (route.tag) params.set("tag", route.tag);
    if (route.starred) params.set("starred", "1");
  }

  if (route.screen === "viewer") {
    if (route.file) params.set("file", route.file);
    params.set("from", formatFrom(route.from));
    if (route.at > 0) params.set("at", String(route.at));
  }

  if (route.screen === "sharing") {
    if (route.tab !== "received") params.set("tab", route.tab);
    if (route.share) params.set("share", route.share);
  }

  if (route.screen === "settings") {
    if (route.settingsTab !== "trash") params.set("settings", route.settingsTab);
  }

  if (route.inspector) {
    params.set("inspector", "1");
    if (route.pane !== "info") params.set("pane", route.pane);
  }

  const query = params.toString();
  return query ? `/?${query}` : "/";
}

/**
 * The listener signature. `pop` is true when the entry came from Back/Forward rather than
 * from the app's own `navigate`, which matters because only the former should restore scroll.
 */
export type RouteListener = (route: Route, pop: boolean) => void;

/**
 * createRouter owns the mapping between the URL and the route.
 *
 * It is deliberately not a React hook: the same instance is read by the shell, written by
 * every view, and must be stable across renders. `subscribe` is what React subscribes to.
 */
export class Router {
  private listeners = new Set<RouteListener>();
  private current: Route;
  private lastUrl: string;

  constructor() {
    this.current = parse(new URLSearchParams(location.search));
    this.lastUrl = location.pathname + location.search;
    window.addEventListener("popstate", this.onPop);
  }

  get route(): Route {
    return this.current;
  }

  subscribe(listener: RouteListener): () => void {
    this.listeners.add(listener);
    return () => this.listeners.delete(listener);
  }

  /** navigate pushes a history entry. Use it for anything the Back button should undo. */
  navigate(next: Route): void {
    this.commit(next, "push");
  }

  /**
   * update replaces the current entry.
   *
   * A no-op when nothing in the URL changes, so an effect that calls it on every render cannot
   * flood the history or the listeners.
   */
  update(next: Route): void {
    this.commit(next, "replace");
  }

  private commit(next: Route, mode: "push" | "replace"): void {
    const url = serialize(next);
    this.current = next;
    if (url !== this.lastUrl) {
      this.lastUrl = url;
      if (mode === "push") history.pushState(null, "", url);
      else history.replaceState(null, "", url);
    }
    for (const listener of this.listeners) listener(next, false);
  }

  private onPop = (): void => {
    const url = location.pathname + location.search;
    this.lastUrl = url;
    this.current = parse(new URLSearchParams(location.search));
    for (const listener of this.listeners) listener(this.current, true);
  };

  destroy(): void {
    window.removeEventListener("popstate", this.onPop);
    this.listeners.clear();
  }
}

/** routeOf is the pure helper views use to derive a new route from the current one. */
export function routeOf(base: Route, changes: Partial<Route>): Route {
  return { ...base, ...changes };
}
