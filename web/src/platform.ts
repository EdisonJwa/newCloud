// Platform-aware shortcut labels.
//
// Every visible shortcut label in the interface comes from here. Printing ⌘ on Windows while
// binding Ctrl is not a cosmetic bug: the interface advertises a key its own handler only
// partly honours, and the person concludes the shortcut does not work rather than that the
// label is wrong. The handler accepts both modifiers everywhere; this decides what the label
// says.

const MAC = /mac|iphone|ipad|ipod/i.test(
  typeof navigator === "undefined" ? "" : navigator.platform || navigator.userAgent,
);

export function isMac(): boolean {
  return MAC;
}

/**
 * shortcut renders one labelled combination, e.g. `shortcut("K")` → "⌘K" or "Ctrl K".
 *
 * The separator differs on purpose: macOS writes the glyphs closed up, Windows and Linux write
 * words with a space. Both are the conventions a reader already knows.
 */
export function shortcut(key: string): string {
  return MAC ? `⌘${key}` : `Ctrl ${key}`;
}

/** alternate is the "other" modifier, used where a second binding exists (Shift+F10). */
export function shiftShortcut(key: string): string {
  return MAC ? `⇧${key}` : `Shift ${key}`;
}

/**
 * primaryModifier reports whether an event carries the platform's primary modifier.
 *
 * Both are accepted regardless of platform: a Windows keyboard on a Mac, a Mac keyboard in a
 * Linux VM and a person who simply reaches for Ctrl are all ordinary. The label names the
 * local convention; the handler does not police it.
 */
export function primaryModifier(e: KeyboardEvent | React.KeyboardEvent): boolean {
  return e.metaKey || e.ctrlKey;
}
