/**
 * The theme contract, in one place.
 *
 * Two independent readers agree on these values: the root layout's inline
 * bootstrap (which must run before first paint, so a saved theme never flashes
 * the wrong palette) and components/theme-toggle.ts. An inline script cannot
 * import, so the layout interpolates these into the script source at SSR.
 * That is a value flowing from one declaration rather than a second copy of
 * it, and it is the difference between a rename that fails loudly and one that
 * silently stops restoring the reader's choice.
 */

export type Theme = 'system' | 'light' | 'dark';

export const THEME_STORAGE_KEY = 'pilots_theme';

/**
 * The two values that may appear in `<html data-theme>`. `system` is the
 * ABSENCE of the attribute, not a third value: the stylesheet's default
 * (`color-scheme: light dark` on :root) is what follows the OS.
 */
export const FORCED_THEMES = ['light', 'dark'] as const;

export function readTheme(stored: string | null | undefined): Theme {
  return stored === 'light' || stored === 'dark' ? stored : 'system';
}
