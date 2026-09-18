/**
 * Where a service form returns to after it succeeds.
 *
 * A service's forms render in two places: the full-width page at
 * `/services/<id>` and the slide-over on the app's canvas. An action that
 * always redirected to the page would throw a reader off the canvas every
 * time they saved a setting. So each form carries a hidden `back`, the
 * action reads it through the same local-path rule every redirect in this
 * app obeys, and the toast key rides along as `?ok=`.
 *
 * Browser-safe and pure, so an action and a test both call the same thing.
 */

import { localPath } from '#lib/utils/local-path.ts';

export function backTo(formData: FormData, fallback: string, ok: string): string {
  const path = localPath(formData.get('back'), fallback);
  const url = new URL(path, 'http://local');
  url.searchParams.set('ok', ok);
  return url.pathname + url.search;
}
