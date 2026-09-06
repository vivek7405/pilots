/**
 * <command-palette>: Ctrl K, then type the name of anything.
 *
 * The app has three lists and a growing number of detail pages, and reaching a
 * named machine took a nav click, a scroll and a scan. This is the shortcut
 * every tool this competes with has, and it reaches services, machines and
 * pages by the same query.
 *
 * The document keydown listener is the legitimate case the skill names: a
 * global shortcut has no element to dispatch from. It reads only the key and
 * the event target, and writes only this element's own state.
 *
 * Results come from a `'use server'` GET query rather than a list shipped to
 * the browser: an org with a thousand machines should not send a thousand rows
 * to filter three out of them.
 */

import { WebComponent, html, prop, navigate } from '@webjsdev/core';
import type { Route } from '@webjsdev/core';
import { inputClass } from '#components/ui/input.ts';
import { kbdClass, kbdGroupClass } from '#components/ui/kbd.ts';
import { badgeClass } from '#components/ui/badge.ts';
import { cn } from '#lib/utils/cn.ts';
import { searchOrg } from '#modules/orgs/queries/search-org.server.ts';
import type { SearchHit } from '#modules/orgs/utils/search.ts';

const KIND_LABEL: Record<string, string> = {
  service: 'service',
  machine: 'machine',
  page: 'page',
};

export class CommandPalette extends WebComponent({
  open: prop(Boolean, { state: true }),
  query: prop(String, { state: true }),
  hits: prop<SearchHit[]>(Array, { state: true }),
  active: prop(Number, { state: true }),
}) {
  /** Guards against an older search landing after a newer one. */
  #seq = 0;

  constructor() {
    super();
    this.open = false;
    this.query = '';
    this.hits = [];
    this.active = 0;
  }

  #onKeydown = (event: KeyboardEvent) => {
    if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === 'k') {
      event.preventDefault();
      this.open ? this.hide() : this.show();
      return;
    }
    if (!this.open) return;
    if (event.key === 'Escape') {
      event.preventDefault();
      this.hide();
      return;
    }
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault();
      const step = event.key === 'ArrowDown' ? 1 : -1;
      const count = this.hits.length;
      if (count > 0) this.active = (this.active + step + count) % count;
      return;
    }
    if (event.key === 'Enter') {
      const hit = this.hits[this.active];
      if (!hit) return;
      event.preventDefault();
      this.go(hit);
    }
  };

  connectedCallback() {
    super.connectedCallback();
    document.addEventListener('keydown', this.#onKeydown);
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    document.removeEventListener('keydown', this.#onKeydown);
  }

  show = (): void => {
    this.open = true;
    this.active = 0;
    void this.search(this.query);
    // After the render that makes the input exist.
    void this.updateComplete.then(() => this.querySelector('input')?.focus());
  };

  hide = (): void => {
    this.open = false;
  };

  private go(hit: SearchHit): void {
    this.hide();
    navigate(hit.href as Route);
  }

  private async search(q: string): Promise<void> {
    const mine = ++this.#seq;
    const result = await searchOrg({ q }).catch(() => []);
    // A slower earlier search must not overwrite a faster later one.
    if (mine !== this.#seq) return;
    this.hits = Array.isArray(result) ? result : [];
    this.active = 0;
  }

  #onInput = (event: Event) => {
    this.query = (event.target as HTMLInputElement).value;
    void this.search(this.query);
  };

  render() {
    if (!this.open) {
      // The trigger is the whole element when closed, so the shortcut is
      // discoverable rather than folklore.
      return html`<button
        type="button"
        class="flex items-center gap-2 rounded-md border border-border bg-background px-2 py-1 text-xs text-muted-foreground hover:text-foreground"
        aria-label="Search this organisation"
        @click=${this.show}
      >
        <span>Search</span>
        <span class=${kbdGroupClass()} role="img" aria-label="Control K">
          <kbd class=${kbdClass()}>Ctrl</kbd><kbd class=${kbdClass()}>K</kbd>
        </span>
      </button>`;
    }

    return html`
      <div class="fixed inset-0 z-50 flex items-start justify-center bg-background/70 p-4 pt-24" @click=${this.hide}>
        <div
          role="dialog"
          aria-modal="true"
          aria-label="Search this organisation"
          class="w-full max-w-lg overflow-hidden rounded-lg border border-border bg-popover shadow-lg"
          @click=${(e: Event) => e.stopPropagation()}
        >
          <label class="sr-only" for="palette-input">Search services, machines and pages</label>
          <input
            id="palette-input"
            type="search"
            placeholder="Search services, machines and pages"
            .value=${this.query}
            class=${cn(inputClass(), 'w-full rounded-none border-0 border-b border-border')}
            @input=${this.#onInput}
          >
          <ul role="listbox" aria-label="Results" class="m-0 max-h-80 list-none overflow-y-auto p-1">
            ${this.hits.length === 0
              ? html`<li class="px-3 py-6 text-center text-sm text-muted-foreground">Nothing matches that.</li>`
              : this.hits.map(
                  (hit, index) => html`
                    <li
                      role="option"
                      aria-selected=${index === this.active ? 'true' : 'false'}
                      class=${cn(
                        'flex cursor-pointer items-center gap-2 rounded-md px-3 py-2 text-sm',
                        index === this.active ? 'bg-accent text-accent-foreground' : '',
                      )}
                      @mouseenter=${() => (this.active = index)}
                      @click=${() => this.go(hit)}
                    >
                      <span class="min-w-0 flex-1 truncate">${hit.label}</span>
                      ${hit.detail
                        ? html`<span class="text-xs text-muted-foreground">${hit.detail}</span>`
                        : ''}
                      <span class=${badgeClass({ variant: 'outline' })}>${KIND_LABEL[hit.kind]}</span>
                    </li>
                  `,
                )}
          </ul>
        </div>
      </div>
    `;
  }
}
CommandPalette.register('command-palette');
