/**
 * What the command palette matches, and how many of each it returns.
 *
 * Pure and separate from the query that feeds it, so the ranking can be
 * asserted without a request scope. The query around it does exactly two
 * things this cannot: it reads the org from the SESSION, and it fetches.
 */

import { NOUN, stateLabel } from '#lib/vocabulary.ts';

export interface SearchHit {
  /**
   * `sandbox` and `instance` are one engine object with two products behind
   * it: a machine that belongs to no service is a sandbox a person opened, and
   * one that does is a copy of their service. A palette that called both
   * "machine" made the two indistinguishable in the one place a person goes
   * when they already know what they are looking for.
   */
  kind: 'service' | 'sandbox' | 'instance' | 'page';
  id: string;
  label: string;
  /** The second line: an app group, a state, or nothing. */
  detail?: string;
  href: string;
}

export interface Named {
  id: string;
  name?: string;
  app?: string;
  state?: string;
  /** Present on a machine that is a copy of a service, absent on a sandbox. */
  service_id?: string;
}

/** The static destinations, matched by the same query as everything else. */
export const PAGES: SearchHit[] = [
  { kind: 'page', id: 'apps', label: NOUN.Apps, href: '/' },
  { kind: 'page', id: 'new', label: 'New app', href: '/services/new' },
  { kind: 'page', id: 'sandboxes', label: NOUN.Sandboxes, href: '/sandboxes' },
  { kind: 'page', id: 'storage', label: NOUN.Storage, href: '/storage' },
  { kind: 'page', id: 'domains', label: NOUN.Domains, href: '/domains' },
  { kind: 'page', id: 'usage', label: NOUN.Usage, href: '/usage' },
  { kind: 'page', id: 'keys', label: NOUN.Tokens, href: '/keys' },
  { kind: 'page', id: 'org', label: NOUN.Team, href: '/org' },
];

/** How many of each kind come back. A palette is a shortcut, not a list page. */
export const PER_KIND = 8;

export function rankHits(services: Named[], machines: Named[], query: string): SearchHit[] {
  const needle = query.trim().toLowerCase();

  const hits: SearchHit[] = [
    ...services.map(
      (s): SearchHit => ({
        kind: 'service',
        id: s.id,
        label: s.name ?? s.id,
        ...(s.app ? { detail: s.app } : {}),
        href: `/services/${s.id}`,
      }),
    ),
    ...machines.map(
      (m): SearchHit => ({
        kind: m.service_id ? 'instance' : 'sandbox',
        id: m.id,
        // The state as the word a person reads, never the engine's own value.
        ...(m.state ? { detail: stateLabel(m.state).word } : {}),
        label: m.name || m.id,
        href: `/machines/${m.id}`,
      }),
    ),
    ...PAGES,
  ];

  const matched = needle
    ? hits.filter((h) => `${h.label} ${h.detail ?? ''} ${h.id}`.toLowerCase().includes(needle))
    : hits;

  // Capped PER KIND rather than overall, so a hundred matching machines cannot
  // push every service off the end of the list.
  const out: SearchHit[] = [];
  for (const kind of ['service', 'instance', 'sandbox', 'page'] as const) {
    out.push(...matched.filter((h) => h.kind === kind).slice(0, PER_KIND));
  }
  return out;
}
