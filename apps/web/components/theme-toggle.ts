/**
 * <theme-toggle>: cycles system -> light -> dark and back.
 *
 * The design tokens follow `color-scheme`, which `[data-theme]` forces and
 * otherwise inherits from the OS, so writing that attribute is what actually
 * repaints the page. The `.dark` class is kept in sync only because the kit's
 * helpers carry `dark:` variants keyed on it (button, badge, input,
 * native-select and checkbox all do), and a custom variant cannot read
 * `color-scheme`.
 *
 * A light-DOM component styled entirely with Tailwind utilities, so it has no
 * `<style>` block and the class-prefix rule does not apply. It is the only
 * component in the app chrome, and the only reason the layout ships any
 * JavaScript at all; the pages themselves stay inert.
 *
 * The pre-paint script in `app/layout.ts` applies the saved choice before the
 * first frame. This element only writes it, so a hard refresh does not flash.
 */

import { WebComponent, html, prop } from '@webjsdev/core';
import { cn } from '#lib/utils/cn.ts';
import { buttonClass } from '#components/ui/button.ts';

const ORDER = ['system', 'light', 'dark'] as const;
type Theme = (typeof ORDER)[number];

const STORAGE_KEY = 'pilots_theme';

class ThemeToggle extends WebComponent({
  theme: prop(String, { state: true }),
}) {
  constructor() {
    super();
    this.theme = 'system';
  }

  connectedCallback() {
    super.connectedCallback();
    // Read what the pre-paint script already applied rather than re-deriving
    // it, so the button's label cannot disagree with the painted page.
    let saved: string | null = null;
    try {
      saved = localStorage.getItem(STORAGE_KEY);
    } catch {
      // A browser with storage blocked still gets a working toggle for this
      // page view; only the memory of the choice is lost.
    }
    this.theme = saved === 'light' || saved === 'dark' ? saved : 'system';
  }

  private cycle() {
    const next = ORDER[(ORDER.indexOf(this.theme as Theme) + 1) % ORDER.length];
    this.theme = next;
    try {
      if (next === 'system') localStorage.removeItem(STORAGE_KEY);
      else localStorage.setItem(STORAGE_KEY, next);
    } catch {
      // See connectedCallback: the toggle still works, it just will not persist.
    }
    apply(next);
  }

  render() {
    const theme = this.theme as Theme;
    const label = theme === 'system' ? 'follows your system' : theme;
    return html`
      <button
        type="button"
        class=${cn(buttonClass({ variant: 'ghost', size: 'icon-sm' }), 'text-muted-foreground hover:text-foreground')}
        @click=${() => this.cycle()}
        aria-label=${`Theme: ${label}. Change it.`}
        title=${`Theme: ${label}`}
      >
        ${ICONS[theme]}
      </button>
    `;
  }
}

/** Write the choice to `<html>`. Exported so the browser test drives the real one. */
export function apply(theme: Theme): void {
  const el = document.documentElement;
  if (theme === 'system') delete el.dataset.theme;
  else el.dataset.theme = theme;
  const dark = theme === 'dark' || (theme === 'system' && matchMedia('(prefers-color-scheme: dark)').matches);
  el.classList.toggle('dark', dark);
}

const ICONS: Record<Theme, unknown> = {
  light: html`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" aria-hidden="true"><circle cx="12" cy="12" r="4" /><path d="M12 3v2M12 19v2M4.93 4.93l1.41 1.41M17.66 17.66l1.41 1.41M3 12h2M19 12h2M4.93 19.07l1.41-1.41M17.66 6.34l1.41-1.41" /></svg>`,
  dark: html`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8Z" /></svg>`,
  system: html`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 5h18v11H3zM8 20h8M12 16v4" /></svg>`,
};

ThemeToggle.register('theme-toggle');
