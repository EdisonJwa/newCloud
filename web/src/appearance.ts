// Appearance preferences.
//
// The design system scopes every token to `.serein-root` and switches appearance on data
// attributes rather than on `:root`, so a host page cannot be restyled by accident. That
// contract decides the shape of this module: the attributes live on `<body class="serein-root">`,
// and the framework's own selectors do the rest.
//
// The important property is that **"follow the system" is the absence of an attribute**, not a
// value equal to the system's answer at load time. The tokens are written that way
// (`@media (prefers-color-scheme: dark) .serein-root:not([data-theme])`), which means:
//
//   * with the default preference, the first paint is already correct — no flash of light
//     before a dark page;
//   * a system appearance change updates the page live without listening for anything;
//   * an explicit choice is a deliberate override, and there is exactly one place it is
//     written.
//
// Applying prefs from a module imported first by `main.tsx` keeps that true for an explicit
// choice too: module scripts run after the stylesheets are parsed and before first paint. It
// cannot be an inline `<script>` in index.html because the server sends `default-src 'self'`
// and an inline script would be refused.

export type ThemePref = "auto" | "light" | "dark";
export type ContrastPref = "auto" | "normal" | "more";
export type TextSizePref = "normal" | "large" | "x-large";
export type MotionPref = "auto" | "reduced";

export interface Prefs {
  theme: ThemePref;
  contrast: ContrastPref;
  textsize: TextSizePref;
  motion: MotionPref;
}

/** The choice the "Larger text" toggle writes. Two steps, because three is a slider nobody reads. */
export const TEXT_SIZES: readonly TextSizePref[] = ["normal", "large"];

const KEYS = {
  theme: "wc.appearance.theme",
  contrast: "wc.appearance.contrast",
  textsize: "wc.appearance.textsize",
  motion: "wc.appearance.motion",
} as const;

export const DEFAULT_PREFS: Prefs = {
  theme: "auto",
  contrast: "auto",
  textsize: "normal",
  motion: "auto",
};

function read(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    // Private mode, or storage disabled by policy. The preference simply does not persist;
    // it still applies for this page.
    return null;
  }
}

function write(key: string, value: string): void {
  try {
    if (value) localStorage.setItem(key, value);
    else localStorage.removeItem(key);
  } catch {
    /* not persisted; not an error the person can act on */
  }
}

function oneOf<T extends string>(value: string | null, allowed: readonly T[], fallback: T): T {
  return (allowed as readonly string[]).includes(value ?? "") ? (value as T) : fallback;
}

export function readPrefs(): Prefs {
  return {
    theme: oneOf(read(KEYS.theme), ["auto", "light", "dark"] as const, DEFAULT_PREFS.theme),
    contrast: oneOf(read(KEYS.contrast), ["auto", "normal", "more"] as const, DEFAULT_PREFS.contrast),
    textsize: oneOf(read(KEYS.textsize), ["normal", "large", "x-large"] as const, DEFAULT_PREFS.textsize),
    motion: oneOf(read(KEYS.motion), ["auto", "reduced"] as const, DEFAULT_PREFS.motion),
  };
}

/**
 * applyPrefs writes the attributes the token layer reads.
 *
 * A preference equal to "auto" removes the attribute rather than writing the resolved value:
 * that is what lets the stylesheet honour a system change and what makes the first paint
 * correct before this function runs.
 */
export function applyPrefs(prefs: Prefs): void {
  const root = document.body;
  if (!root) return;

  set(root, "theme", prefs.theme, "auto");
  set(root, "contrast", prefs.contrast, "auto");
  set(root, "textsize", prefs.textsize, "normal");
  set(root, "motion", prefs.motion, "auto");

  // `color-scheme` follows the resolved appearance so native controls (scrollbars, the
  // `<select>` popup, form widgets) agree with the page. The token sheet sets it on the root
  // already; this keeps it right when the OS is dark and an explicit light choice is made.
  root.style.colorScheme = "";
}

function set(root: HTMLElement, name: string, value: string, neutral: string): void {
  if (value === neutral) delete root.dataset[name];
  else root.dataset[name] = value;
}

export function savePrefs(prefs: Prefs): void {
  write(KEYS.theme, prefs.theme === "auto" ? "" : prefs.theme);
  write(KEYS.contrast, prefs.contrast === "auto" ? "" : prefs.contrast);
  write(KEYS.textsize, prefs.textsize === "normal" ? "" : prefs.textsize);
  write(KEYS.motion, prefs.motion === "auto" ? "" : prefs.motion);
}

/** The appearance actually in effect, for the menu's current-value label. */
export function resolveTheme(pref: ThemePref): "light" | "dark" {
  if (pref !== "auto") return pref;
  return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

/** The contrast actually in effect. Exported for the same reason as `resolveTheme`. */
export function resolveContrast(pref: ContrastPref): "normal" | "more" {
  if (pref !== "auto") return pref;
  return window.matchMedia("(prefers-contrast: more)").matches ? "more" : "normal";
}
