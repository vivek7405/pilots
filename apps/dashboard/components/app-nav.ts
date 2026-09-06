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
import { NOUN } from '#lib/vocabulary.ts';
import { cn } from '#lib/utils/cn.ts';

// Module scope so the value survives a re-render. Written on the client only,
// so SSR never touches `location`, which does not exist there.
const activePath = signal('');

/**
 * The product nouns, and only those.
 *
 * Tokens, Team and Usage are account chores and live in the identity menu;
 * Storage and Domains are attributes of a service and stay routable, reached
 * from a service rather than from here. A nav of seven equal items said
 * nothing about what this product is for.
 *
 * The words are the user's, from `lib/vocabulary.ts`. `Apps` rather than
 * `Overview` because the root IS the list of apps rather than a summary of
 * something else, and `Sandboxes` rather than `Machines` because a machine is
 * an engine word for two different products: an instance inside a service and
 * a sandbox that belongs to no service.
 *
 * Logs joins this list when /logs exists. A nav entry pointing at a 404 is
 * worse than one missing entry.
 */
const LINKS: { href: string; label: string }[] = [
  { href: '/', label: NOUN.Apps },
  { href: '/sandboxes', label: NOUN.Sandboxes },
  { href: '/logs', label: NOUN.Logs },
  { href: '/sandboxes/playground', label: 'Playground' },
];

/**
 * Paths a nav entry owns that do not sit under it.
 *
 * A URL segment is an address and `/machines/<id>` keeps its path, so a
 * sandbox's own page does not live under `/sandboxes`. Without this the nav
 * goes dark the moment you open one, which reads as having left the section
 * you are plainly still in. Likewise a service's page belongs to Apps, which
 * is where the service was found.
 */
const OWNS: Record<string, string[]> = {
  '/sandboxes': ['/machines'],
  '/sandboxes/playground': [],
  '/': ['/apps', '/services'],
};

export class AppNav extends WebComponent({ current: prop(String), orientation: prop(String) }) {
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
    const vertical = this.orientation === 'vertical';
    return html`
      <nav
        class=${cn(
          vertical ? 'flex flex-col gap-1' : 'flex items-center gap-0.5 overflow-x-auto sm:gap-1',
        )}
        aria-label="Primary"
      >
        ${LINKS.map((link) => {
          // A section owns its subroutes, so /apps/<app> keeps Apps lit. '/'
          // is exact, or it would match every path, and it owns the paths in
          // OWNS instead.
          const under = (base: string) => active === base || active.startsWith(base + '/');
          const owned = (OWNS[link.href] ?? []).some(under);
          const on = link.href === '/' ? active === '/' || owned : under(link.href) || owned;
          return html`<a
            href=${link.href}
            aria-current=${on ? 'page' : 'false'}
            class=${cn(
              'no-underline transition-colors',
              vertical
                ? 'flex items-center rounded-md px-3 py-2 text-body'
                : 'rounded-md px-2 py-1.5 text-body sm:px-3',
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
