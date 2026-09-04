/**
 * The period an export or a page covers, browser-safe so the form and the
 * route agree on the defaults.
 *
 * The default is the current calendar month, because that is the unit a bill
 * is drawn on and the one a reader is looking for when they open the page with
 * no dates.
 */

export interface Period {
  since: Date;
  until: Date;
}

export function resolvePeriod(rawSince?: string | null, rawUntil?: string | null, now = new Date()): Period {
  const since = parseDay(rawSince) ?? new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), 1));
  const until = parseDay(rawUntil) ?? new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth() + 1, 1));
  // An inverted range is a typo, not a query: it would silently return nothing.
  return until > since ? { since, until } : { since, until: new Date(since.getTime() + 86_400_000) };
}

/** `YYYY-MM-DD`, or null for anything else. */
function parseDay(raw?: string | null): Date | null {
  if (!raw || !/^\d{4}-\d{2}-\d{2}$/.test(raw)) return null;
  const parsed = new Date(`${raw}T00:00:00.000Z`);
  return Number.isNaN(parsed.getTime()) ? null : parsed;
}
