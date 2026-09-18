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
 */
import type { LayoutProps } from '@webjsdev/core';
import SiteLayout from './(site)/layout.ts';
import NotFound from './(site)/not-found.ts';

export default function RootNotFound(props: LayoutProps) {
  return SiteLayout({ ...props, children: NotFound() });
}
