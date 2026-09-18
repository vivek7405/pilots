/**
 * <org-switcher>: submits the org-switch form when a menu radio is chosen.
 *
 * The identity menu shows the orgs as radio items because that is what a
 * choice among mutually exclusive options is, and a radio item cannot post
 * anything on its own. This wraps the menu, listens for the kit's
 * `ui-item-select`, and submits the real `switchOrg` form the layout rendered
 * beside it, with the chosen id and the current path to come back to.
 *
 * With scripting off the same form is on /org under Account, which is why this
 * element upgrades a menu rather than being the only way to switch.
 */

import { WebComponent, html } from '@webjsdev/core';

interface ItemSelectDetail {
  value: string;
  type: string;
}

export class OrgSwitcher extends WebComponent {
  #onSelect = (event: Event) => {
    const detail = (event as CustomEvent<ItemSelectDetail>).detail;
    if (!detail || detail.type !== 'radio' || !detail.value) return;
    const form = this.querySelector<HTMLFormElement>('form[data-org-switch]');
    if (!form) return;
    const org = form.elements.namedItem('org');
    const back = form.elements.namedItem('back');
    if (org instanceof HTMLInputElement) org.value = detail.value;
    if (back instanceof HTMLInputElement) back.value = location.pathname + location.search;
    form.requestSubmit();
  };

  connectedCallback() {
    super.connectedCallback();
    this.addEventListener('ui-item-select', this.#onSelect);
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    this.removeEventListener('ui-item-select', this.#onSelect);
  }

  render() {
    // Light DOM passthrough: the menu and the form are the layout's markup, so
    // they stay server-rendered and this element only wires them together.
    return html`<slot></slot>`;
  }
}
OrgSwitcher.register('org-switcher');
