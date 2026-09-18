/**
 * <link-rows>: makes whole table rows navigate.
 *
 * A row whose only link is the name cell is a 40-pixel target in a
 * 1000-pixel-wide row, and the dashboard's own AGENTS.md asks for the whole
 * row. The name cell stays a real `<a>`, so a middle click, a modifier click
 * and "copy link address" all still work and nothing here is the only way in.
 *
 * A click that landed on a control is left alone. That is the whole reason
 * this is a wrapper rather than a click handler on the row: the destructive
 * buttons in these tables sit inside the click target, and a Destroy that also
 * navigates is a Destroy nobody can read the confirmation of.
 */

import { WebComponent, html, navigate } from '@webjsdev/core';
import type { Route } from '@webjsdev/core';

const INTERACTIVE = 'a, button, input, select, textarea, label, summary, [role="button"]';

export class LinkRows extends WebComponent {
  #onClick = (event: MouseEvent) => {
    // A modified click means "somewhere else": let the browser do its thing.
    if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) {
      return;
    }
    const target = event.target as HTMLElement | null;
    if (!target || target.closest(INTERACTIVE)) return;
    // A drag to select text should not navigate.
    if ((globalThis.getSelection?.()?.toString() ?? '').length > 0) return;
    const href = target.closest<HTMLElement>('[data-href]')?.dataset.href;
    if (!href) return;
    event.preventDefault();
    // `navigate` is typed against the app's own route union, which is exactly
    // what makes it useless here: this component is generic over whatever
    // table it wraps, and the href came out of an attribute at runtime. A bad
    // path is a 404 the router renders, not a type error it could have caught.
    navigate(href as Route);
  };

  connectedCallback() {
    super.connectedCallback();
    this.addEventListener('click', this.#onClick);
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    this.removeEventListener('click', this.#onClick);
  }

  render() {
    return html`<slot></slot>`;
  }
}
LinkRows.register('link-rows');
