/**
 * An address nothing else in the app matches.
 *
 * A catch-all is the lowest-priority match, so this only ever sees a path no
 * page, route handler or metadata route claimed. It throws `notFound()` so the
 * answer is a ROUTED 404: `not-found.ts` beside the layout renders inside the
 * real layout chain, with the layout's component modules shipped. The app's
 * root `app/not-found.ts` cannot do that. The framework renders it with no
 * route, so no module script reaches the page and `<site-theme-toggle>` sits
 * there inert.
 *
 * `/dashboard/<anything>` never arrives here: the product has a catch-all of
 * its own under its prefix, and a static prefix outranks this one.
 */
import { notFound } from '@webjsdev/core';

export default function SiteNotFound(): never {
  throw notFound();
}
