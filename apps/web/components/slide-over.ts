/**
 * <slide-over back="/apps/x">: the panel that opens beside the canvas.
 *
 * Not a `<dialog>` and not `aria-modal`: the canvas has to stay usable
 * behind it, and a modal makes everything else inert. The panel is fixed
 * under the header, scrolls on its own, and projects the page's own markup
 * through a slot, so the service's tabs and forms cost the browser nothing
 * and exist with scripting off.
 *
 * With scripting on it adds two things: focus moves to the panel's heading
 * when it opens, and Escape goes back to the canvas. Both work through the
 * URL, because the selection lives there: closing is a navigation, never a
 * hidden attribute, so a reload lands on the same state.
 *
 * Before navigating on Escape it dispatches a cancelable `slide-over-close`
 * event carrying the destination, so a test can observe the intent without
 * the real router leaving the page.
 */

import { WebComponent, html, navigate, prop } from '@webjsdev/core';
import type { Route } from '@webjsdev/core';

export class SlideOver extends WebComponent({ back: prop(String) }) {
  constructor() {
    super();
    this.back = '';
  }

  #onKeydown = (event: KeyboardEvent) => {
    if (event.key !== 'Escape' || !this.back) return;
    const target = event.target instanceof Element ? event.target : null;
    // A terminal owns Escape (vim, less, a shell prompt) and so does a text
    // field; only a bare Escape on the panel itself closes it.
    if (target && (target.closest('machine-terminal') || /^(INPUT|TEXTAREA|SELECT)$/.test(target.tagName))) return;
    const intent = new CustomEvent('slide-over-close', { detail: { back: this.back }, bubbles: true, cancelable: true });
    if (!this.dispatchEvent(intent)) return;
    event.preventDefault();
    navigate(this.back as Route);
  };

  connectedCallback() {
    super.connectedCallback();
    document.addEventListener('keydown', this.#onKeydown);
  }

  firstUpdated() {
    // The heading is slotted content, projected one microtask after the
    // first render, so the focus waits for that projection.
    queueMicrotask(() => this.querySelector<HTMLElement>('h2')?.focus());
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    document.removeEventListener('keydown', this.#onKeydown);
  }

  render() {
    return html`<div
      role="dialog"
      aria-modal="false"
      aria-labelledby="service-panel-title"
      class="fixed right-0 bottom-0 z-30 w-full max-w-2xl overflow-y-auto border-l border-border bg-card shadow-xl"
      style="top: var(--header-h)"
    >
      <slot></slot>
    </div>`;
  }
}
SlideOver.register('slide-over');
