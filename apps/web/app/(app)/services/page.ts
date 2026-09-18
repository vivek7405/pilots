/**
 * `/services` is now `/`.
 *
 * The root used to be an overview that summarised the services list below it,
 * so the two pages answered the same question twice. The root is now the list
 * of apps, which is where a service is found, so this address folds into it.
 *
 * A 308 for the reason `/machines` uses one: the path was linked from the nav
 * and bookmarked, and a permanent redirect is what says it moved.
 */
import { redirect } from '@webjsdev/core';

export default function ServicesRedirect(): never {
  throw redirect('/', 308);
}
