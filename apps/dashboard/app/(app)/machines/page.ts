/**
 * `/machines` is now `/sandboxes`.
 *
 * A 308 rather than a rewrite: the old path was linked from the nav, from the
 * command palette and from anywhere a person bookmarked it, and a permanent
 * redirect is what tells a browser and a crawler that the address moved rather
 * than that the page is temporarily elsewhere.
 *
 * `/machines/<id>` and its terminal keep their paths. A URL segment is an
 * address, not a word this app says to anyone, and moving one breaks every
 * link that was ever copied out of the address bar for no gain.
 */
import { redirect } from '@webjsdev/core';

export default function MachinesRedirect(): never {
  throw redirect('/sandboxes', 308);
}
