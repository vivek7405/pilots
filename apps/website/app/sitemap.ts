import { sitemap } from '@webjsdev/server';
import { SITE_ORIGIN } from '#lib/links.ts';

/**
 * /sitemap.xml
 *
 * The site is small and every route is hand-authored, so the list is written
 * out rather than derived from a content query. When a route stops being
 * hand-authored (a docs tree, a changelog), this switches to enumerating from
 * disk; until then a generated list would be indirection with nothing behind
 * it.
 *
 * Priorities reflect what the site is FOR. /architecture sits just under the
 * home page because it is the page that does the convincing, and it is the one
 * a skeptical reader is most likely to land on from a link someone shared.
 */
const PRIORITY: Record<string, number> = {
  '/': 1.0,
  '/architecture': 0.9,
  '/sandboxes': 0.8,
  '/deploy': 0.8,
  '/roadmap': 0.6,
};

export default function Sitemap() {
  return sitemap(
    Object.keys(PRIORITY).map((path) => ({
      url: `${SITE_ORIGIN}${path}`,
      changeFrequency: 'weekly' as const,
      priority: PRIORITY[path],
    })),
  );
}
