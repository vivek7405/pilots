/**
 * The trail in the header, from the URL alone.
 *
 * A pure function of the path, because the root layout is PRESERVED across a
 * client-router navigation and has no access to the page's own data. It cannot
 * fetch and it must not: the header renders on every screen and a query there
 * would be a query on every screen.
 *
 * That constraint settles one thing the plan left implied. `?service=` carries
 * an ID, and an id is not a name, so the trail stops at the app rather than
 * ending in `svc_9f2c…`. The rule is: **a crumb is a name or a section, never
 * an id.** An app name is a name, so it renders; a service id is not, so the
 * panel's own `h2` right below carries the service instead, which is where a
 * reader looks anyway. A trail whose last item is an opaque string teaches
 * nothing and costs a whole row of chrome.
 *
 * The last crumb is the current page and is not a link. A trail whose last
 * item links to where you already are is a control that does nothing.
 */

import { DASHBOARD } from '#lib/paths.ts';

export interface Crumb {
  label: string;
  /** Absent on the last crumb, which is the page you are on. */
  href?: string;
}

/** A path segment as the section name a person would recognise. */
const SECTION: Record<string, { label: string; href: string }> = {
  sandboxes: { label: 'Sandboxes', href: '/dashboard/sandboxes' },
  machines: { label: 'Sandboxes', href: '/dashboard/sandboxes' },
  storage: { label: 'Storage', href: '/dashboard/storage' },
  domains: { label: 'Domains', href: '/dashboard/domains' },
  logs: { label: 'Logs', href: '/dashboard/logs' },
  usage: { label: 'Usage', href: '/dashboard/usage' },
  keys: { label: 'Tokens', href: '/dashboard/keys' },
  org: { label: 'Team', href: '/dashboard/org' },
  services: { label: 'Apps', href: DASHBOARD },
};

export function breadcrumb(path: string): Crumb[] {
  const all = path.split('/').filter(Boolean);
  // The prefix is where the product lives, not a place in it: it never renders
  // as a crumb of its own, and the root crumb is what links to it.
  const parts = all[0] === DASHBOARD.slice(1) ? all.slice(1) : all;
  if (parts.length === 0) return [{ label: 'pilots' }];

  const trail: Crumb[] = [{ label: 'pilots', href: DASHBOARD }];
  const [first, second, third] = parts;

  // An app name IS a name, so it is the one dynamic segment that renders.
  if (first === 'apps' && second) {
    trail.push({ label: decodeURIComponent(second) });
    return trail;
  }

  if (first === 'services' && second === 'new') {
    trail.push({ label: 'New app' });
    return trail;
  }

  const section = first ? SECTION[first] : undefined;
  if (!section) return trail;

  // A detail page under a section: the section links, and the leaf is the
  // section's own singular rather than the id in the address bar.
  if (second) {
    trail.push(section);
    if (first === 'machines' && third === 'terminal') {
      trail.push({ label: 'Terminal' });
    } else if (first === 'machines') {
      trail.push({ label: 'Sandbox' });
    } else if (first === 'services') {
      trail.push({ label: 'Service' });
    }
    return trail;
  }

  trail.push({ label: section.label });
  return trail;
}
