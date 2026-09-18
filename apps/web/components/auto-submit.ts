/**
 * <auto-submit>: a GET form that submits itself when a control changes.
 *
 * The app list's sort is a plain form so it works with scripting off, and
 * with scripting on nobody wants to pick an option and then press a button.
 * This wraps the form, submits it on `change`, and hides the button that a
 * no-script visitor still needs.
 *
 * The form is the page's own markup, projected through a slot: only the
 * listener ships.
 */

import { WebComponent, html } from '@webjsdev/core';

export class AutoSubmit extends WebComponent {
  #onChange = (event: Event) => {
    const form = (event.target as HTMLElement | null)?.closest('form');
    if (form) form.requestSubmit();
  };

  connectedCallback() {
    super.connectedCallback();
    this.addEventListener('change', this.#onChange);
    for (const button of this.querySelectorAll<HTMLElement>('[data-auto-submit-button]')) button.hidden = true;
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    this.removeEventListener('change', this.#onChange);
  }

  render() {
    return html`<slot></slot>`;
  }
}
AutoSubmit.register('auto-submit');
