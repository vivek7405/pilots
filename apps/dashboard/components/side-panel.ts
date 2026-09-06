/**
 * <side-panel>: a collapsible strip beside a full-height pane.
 *
 * It exists for the terminal route, where the panel is useful and the terminal
 * is the point: someone who wants the full width should be able to take it and
 * keep it. The choice is remembered per browser in `localStorage`, which is the
 * right store for a per-viewer layout preference and the wrong one for
 * anything the server needs to know.
 *
 * The panel's CONTENT is the page's markup, projected through a slot, so none
 * of it ships as JavaScript. Only the toggle does.
 */

import { WebComponent, html, prop } from '@webjsdev/core';
import { buttonClass } from '#components/ui/button.ts';
import { cn } from '#lib/utils/cn.ts';

const KEY = 'pilots_side_panel';

export class SidePanel extends WebComponent({
  /** Distinguishes one panel's remembered state from another's. */
  name: prop(String),
  open: prop(Boolean, { state: true }),
}) {
  constructor() {
    super();
    this.name = 'panel';
    // Open at SSR and on the first paint. A panel that starts closed and
    // springs open once the browser reads storage is a visible jump.
    this.open = true;
  }

  connectedCallback() {
    super.connectedCallback();
    try {
      if (localStorage.getItem(`${KEY}:${this.name}`) === 'closed') this.open = false;
    } catch {
      // A browser with storage blocked keeps the default, which is fine.
    }
  }

  private toggle = (): void => {
    this.open = !this.open;
    try {
      localStorage.setItem(`${KEY}:${this.name}`, this.open ? 'open' : 'closed');
    } catch {
      // The panel still toggles; only the memory of it is lost.
    }
  };

  render() {
    return html`
      <div class=${cn('flex h-full', this.open ? 'w-80' : 'w-10')}>
        <button
          type="button"
          class=${cn(buttonClass({ variant: 'ghost', size: 'icon-sm' }), 'mt-2 ml-1 shrink-0')}
          aria-expanded=${this.open ? 'true' : 'false'}
          aria-label=${this.open ? 'Collapse the details panel' : 'Expand the details panel'}
          @click=${this.toggle}
        >
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
            <path d=${this.open ? 'm9 18 6-6-6-6' : 'm15 18-6-6 6-6'} />
          </svg>
        </button>
        <div class=${cn('min-w-0 flex-1 overflow-y-auto', this.open ? '' : 'hidden')}><slot></slot></div>
      </div>
    `;
  }
}
SidePanel.register('side-panel');
