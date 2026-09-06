/**
 * <list-filter for="<table id>">: hides rows that do not match what is typed.
 *
 * Client-side and text-only on purpose. These lists are an org's machines and
 * services, which is tens of rows, so a round trip per keystroke would be
 * slower and would take the list away while it loaded.
 *
 * `/` focuses it, the way it does in every tool this competes with, and only
 * when the visitor is not already typing somewhere. A component that stole `/`
 * from a text field would make every search box in the app unusable.
 */

import { WebComponent, html, prop } from '@webjsdev/core';
import { inputClass } from '#components/ui/input.ts';
import { kbdClass } from '#components/ui/kbd.ts';
import { cn } from '#lib/utils/cn.ts';

export class ListFilter extends WebComponent({
  /** The id of the table whose `tbody` rows this filters. */
  for: prop(String),
  placeholder: prop(String),
  query: prop(String, { state: true }),
}) {
  constructor() {
    super();
    this.for = '';
    this.placeholder = 'Filter';
    this.query = '';
  }

  #onKeydown = (event: KeyboardEvent) => {
    if (event.key !== '/' || event.metaKey || event.ctrlKey || event.altKey) return;
    const target = event.target as HTMLElement | null;
    // Never steal the key from something the visitor is typing into.
    if (target && (target.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(target.tagName))) return;
    event.preventDefault();
    this.querySelector('input')?.focus();
  };

  connectedCallback() {
    super.connectedCallback();
    document.addEventListener('keydown', this.#onKeydown);
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    document.removeEventListener('keydown', this.#onKeydown);
  }

  #onInput = (event: Event) => {
    this.query = (event.target as HTMLInputElement).value;
    this.#apply();
    // A list that owns its own rows (the live machine list) filters in its own
    // render instead of having them hidden underneath it.
    this.dispatchEvent(
      new CustomEvent('list-filter-change', { detail: { query: this.query }, bubbles: true }),
    );
  };

  #apply() {
    if (!this.for) return;
    const table = document.getElementById(this.for);
    if (!table) return;
    const needle = this.query.trim().toLowerCase();
    let shown = 0;
    for (const row of table.querySelectorAll<HTMLElement>('tbody tr')) {
      const hit = needle === '' || (row.textContent ?? '').toLowerCase().includes(needle);
      row.hidden = !hit;
      if (hit) shown += 1;
    }
    const status = this.querySelector('[data-filter-status]');
    if (status) status.textContent = needle === '' ? '' : `${shown} shown`;
  }

  render() {
    const id = `filter-${this.for || 'list'}`;
    return html`
      <div class="flex items-center gap-2">
        <label class="sr-only" for=${id}>${this.placeholder}</label>
        <input
          id=${id}
          type="search"
          placeholder=${this.placeholder}
          .value=${this.query}
          class=${cn(inputClass(), 'h-8 w-56')}
          @input=${this.#onInput}
        >
        <kbd class=${kbdClass()} aria-hidden="true">/</kbd>
        <span data-filter-status class="text-xs text-muted-foreground" role="status" aria-live="polite"></span>
      </div>
    `;
  }
}
ListFilter.register('list-filter');
