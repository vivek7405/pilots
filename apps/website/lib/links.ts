import { html } from '@webjsdev/core';

/**
 * Every URL the site chrome needs, declared once.
 *
 * AGENTS.md invariant 10: the domain is written HERE and nowhere else, and
 * test/no-slop/no-slop.test.ts fails any other file that spells it out. The
 * brand has moved before (see the project's own history), so the next move is
 * a one-line edit rather than a hunt through canonicals, OG tags, sitemap
 * entries, and JSON-LD @ids.
 */

/**
 * The marketing site and the dashboard share this apex. User workloads never
 * do, and that separation is a security boundary rather than a preference: a
 * guest on the dashboard's apex could set cookies scoped to it. Workloads get
 * their own apex below.
 */
export const SITE_ORIGIN = 'https://pilots.run';

/**
 * Where user workloads answer: `<name>.pilotrun.app`, and
 * `<port>-<name>.pilotrun.app` for arbitrary ports. ONE apex for sandboxes and
 * production services alike, because `promote` must not change a URL and a
 * promotion that crossed apexes would.
 */
export const WORKLOAD_APEX = 'pilotrun.app';

export const GH_URL = 'https://github.com/vivek7405/pilots';
export const GH_BOARD_URL = 'https://github.com/users/vivek7405/projects/10';

/**
 * WebJs is the sibling product: the framework, where pilots is the platform
 * it runs on. Same company, the way Next.js and Vercel are the same company.
 * The site links it as a sibling rather than as a third-party integration,
 * because that is what it is.
 */
export const WEBJS_URL = 'https://webjs.dev';

/**
 * The screen-reader cue appended to any link that leaves the site. Sighted
 * readers get the new tab; a screen-reader user gets told it is coming, which
 * is the half that a bare target="_blank" silently drops.
 */
export const NEW_TAB = html`<span class="sr-only"> (opens in a new tab)</span>`;

export const NAV = [
  { label: 'Sandboxes', href: '/sandboxes' },
  { label: 'Deploy', href: '/deploy' },
  { label: 'Architecture', href: '/architecture' },
  { label: 'Roadmap', href: '/roadmap' },
  { label: 'Brand', href: '/brand' },
];
