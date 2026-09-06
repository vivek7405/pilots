/**
 * <app-nav>: the header's primary navigation, with the current section lit.
 *
 * A component rather than plain layout markup for one reason: the root layout
 * is PRESERVED across a client-router navigation, so a highlight computed on
 * the server freezes on whichever page the visitor happened to load first.
 * This re-derives it from the router's own navigate event, and the `current`
 * attribute seeds the first paint so the server's answer is right too.
 *
 * Listening on `document` is the legitimate case the skill names: the router
 * has no element to dispatch from, and this handler reads only
 * `location.pathname` and writes only this module's signal. It queries nothing
 * another component rendered.
 */

import { WebComponent, prop, html, signal } from '@webjsdev/core';
import { cn } from '#lib/utils/cn.ts';

// Module scope so the value survives a re-render. Written on the client only,
// so SSR never touches `location`, which does not exist there.
const activePath = signal('');

/**
 * The product nouns, and only those.
 *
 * Tokens, Team and Usage are account chores and live in the identity menu;
 * Volumes and Domains are attributes of a service and stay routable, reached
 * from a service rather than from here. A nav of seven equal items said
 * nothing about what this product is for.
 *
 * Logs will join this list when /logs exists. A nav entry pointing at a 404 is
 * worse than one missing entry.
 */
const LINKS: { href: string; label: string }[] = [
  { href: '/', label: 'Overview' },
  { href: '/services', label: 'Services' },
  { href: '/machines', label: 'Machines' },
];

export class AppNav extends WebComponent({ current: prop(String) }) {
  #onNav = () => activePath.set(location.pathname);

  connectedCallback() {
    super.connectedCallback();
    this.#onNav(); // seed from the real URL on hydrate
    document.addEventListener('webjs:navigate', this.#onNav);
    window.addEventListener('popstate', this.#onNav);
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    document.removeEventListener('webjs:navigate', this.#onNav);
    window.removeEventListener('popstate', this.#onNav);
  }

  render() {
    const active = activePath.get() || this.current || '/';
    return html`
      <nav class="flex items-center gap-0.5 overflow-x-auto sm:gap-1" aria-label="Primary">
        ${LINKS.map((link) => {
          // A section owns its subroutes, so /services/<id> keeps Services
          // lit. '/' is exact, or it would match every path.
          const on =
            link.href === '/'
              ? active === '/'
              : active === link.href || active.startsWith(link.href + '/');
          return html`<a
            href=${link.href}
            aria-current=${on ? 'page' : 'false'}
            class=${cn(
              'rounded-md px-2 py-1.5 text-sm no-underline transition-colors sm:px-3',
              on
                ? 'bg-accent font-medium text-foreground'
                : 'text-muted-foreground hover:bg-accent/60 hover:text-foreground',
            )}
            >${link.label}</a
          >`;
        })}
      </nav>
    `;
  }
}
AppNav.register('app-nav');
