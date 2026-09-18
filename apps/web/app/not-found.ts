/**
 * The 404 for an address nothing in the app matches.
 *
 * The app has two shells and no root layout, because the marketing site and
 * the product share no chrome. A page's own `notFound()` is answered by its
 * group's not-found inside that group's layout. An address that matches no
 * route at all belongs to neither group, so the framework renders THIS file
 * with no layout around it, which would be an unstyled page.
 *
 * It is the marketing 404 on purpose: `/` is the marketing site, and someone
 * guessing a URL under it is looking at pilots.run, not at their dashboard.
 *
 * This is the FALLBACK, not the usual path. A mistyped address is caught by
 * a catch-all page in each shell (`(site)/[...rest]` and
 * `(product)/dashboard/[...rest]`), which answers a routed 404 inside the
 * real layout chain, component modules included. The framework renders this
 * file with no route, so it ships no module scripts; it is what is left for a
 * `notFound()` raised where there is no route to render it in.
 */
import type { LayoutProps } from '@webjsdev/core';
import SiteLayout from './(site)/layout.ts';
import NotFound from './(site)/not-found.ts';

export default function RootNotFound(props: LayoutProps) {
  return SiteLayout({ ...props, children: NotFound() });
}
