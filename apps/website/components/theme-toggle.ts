import { WebComponent, html, signal } from '@webjsdev/core';
import { THEME_STORAGE_KEY, readTheme, type Theme } from '#lib/theme.ts';

/**
 * `<theme-toggle>`: cycles system → light → dark → system.
 *
 * The label is the current state spelled out in monospace rather than an icon
 * alone. On an instrument panel a switch tells you its position, and a
 * three-state control whose only cue is a glyph makes the reader click twice
 * to find out where they are.
 */
export class ThemeToggle extends WebComponent {
  theme = signal<Theme>('system');

  connectedCallback() {
    super.connectedCallback();
    let saved: string | null = null;
    try { saved = localStorage.getItem(THEME_STORAGE_KEY); } catch {}
    this.theme.set(readTheme(saved));
  }

  cycle() {
    const t = this.theme.get();
    const next: Theme = t === 'system' ? 'light' : t === 'light' ? 'dark' : 'system';
    this.theme.set(next);
    try {
      if (next === 'system') localStorage.removeItem(THEME_STORAGE_KEY);
      else localStorage.setItem(THEME_STORAGE_KEY, next);
    } catch {}
    if (next === 'system') delete document.documentElement.dataset.theme;
    else document.documentElement.dataset.theme = next;
  }

  render() {
    const t = this.theme.get();
    const label = t === 'system' ? 'AUTO' : t === 'light' ? 'LIGHT' : 'DARK';
    return html`
      <button
        class="inline-flex items-center h-8 px-2.5 rounded-[3px] border border-rule bg-transparent
               font-mono text-[10px] tracking-[0.12em] text-ink-subtle cursor-pointer
               transition-colors duration-150 hover:text-ink hover:border-rule-strong"
        @click=${() => this.cycle()}
        aria-label="Cycle theme, currently ${label}"
      >${label}</button>
    `;
  }
}

ThemeToggle.register('theme-toggle');
