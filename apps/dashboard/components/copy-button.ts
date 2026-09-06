/**
 * <copy-button value="..." label="..."> : one click puts a value on the
 * clipboard.
 *
 * Ids and URLs in this app are things a person retypes into a terminal, and
 * retyping `m-0123456789abcdef01234567` by eye is how the wrong machine gets
 * destroyed. The label is required rather than optional because the button
 * shows an icon: without it there is no accessible name at all.
 *
 * It renders at SSR as the same inert button, which is deliberate. A button
 * that appears on hydration makes the row reflow, and the clipboard is a
 * browser API that could never have worked without scripting anyway.
 */

import { WebComponent, html, prop } from '@webjsdev/core';
import { buttonClass } from '#components/ui/button.ts';
import { toast } from '#components/ui/sonner.ts';
import { cn } from '#lib/utils/cn.ts';

export class CopyButton extends WebComponent({
  value: prop(String),
  label: prop(String),
  copied: prop(Boolean, { state: true }),
}) {
  #timer: ReturnType<typeof setTimeout> | undefined;

  constructor() {
    super();
    this.value = '';
    this.label = 'value';
    this.copied = false;
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    clearTimeout(this.#timer);
  }

  #copy = async (event: Event) => {
    // The button often sits inside a row that navigates on click.
    event.stopPropagation();
    event.preventDefault();
    try {
      await navigator.clipboard.writeText(this.value);
      this.copied = true;
      toast.success(`Copied the ${this.label}`);
      clearTimeout(this.#timer);
      this.#timer = setTimeout(() => {
        this.copied = false;
      }, 1500);
    } catch {
      // A denied clipboard permission is the visitor's choice, not a failure
      // to report as one; the value is on screen and can still be selected.
      toast.error('The browser refused clipboard access');
    }
  };

  render() {
    return html`<button
      type="button"
      class=${cn(buttonClass({ variant: 'ghost', size: 'icon-sm' }), 'align-middle text-muted-foreground hover:text-foreground')}
      aria-label=${`Copy the ${this.label}`}
      @click=${this.#copy}
    >
      ${this.copied
        ? html`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M20 6 9 17l-5-5" /></svg>`
        : html`<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect width="14" height="14" x="8" y="8" rx="2" /><path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2" /></svg>`}
    </button>`;
  }
}
CopyButton.register('copy-button');
