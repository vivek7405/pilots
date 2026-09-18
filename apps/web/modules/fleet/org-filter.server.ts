/**
 * Tenancy at the dashboard's edge.
 *
 * hostd enforces tenancy for a scoped key, but this app holds ONE admin key,
 * which by construction sees every row on the fleet. So every id that arrives
 * from a URL is checked against the org the visitor is acting as before the
 * row is rendered or acted on.
 *
 * A foreign id is a 404, never a 403. A 403 confirms the id exists, which
 * turns a guessed id into an existence oracle for another tenant's machines.
 */

import { PilotsError } from '@pilots/sdk';

/** A fleet row that carries the tenant it belongs to. */
export interface Owned {
  org_id?: string;
}

/** The row when this org owns it, else null. */
export function assertOwned<T extends Owned>(orgId: string, row: T | null | undefined): T | null {
  if (!row) return null;
  return row.org_id === orgId ? row : null;
}

/** Rows this org owns. Applied after a list the engine already narrowed. */
export function ownedOnly<T extends Owned>(orgId: string, rows: T[]): T[] {
  return rows.filter((r) => r.org_id === orgId);
}

/**
 * Map an SDK error onto a response.
 *
 * The key never reaches a log line or a response body. A 502 carries the
 * upstream status so an operator can tell a quota refusal from a host that is
 * down, without the body being a channel for whatever hostd said.
 */
export function fleetErrorResponse(err: unknown, log: (message: string) => void = console.error): Response {
  if (isNamed(err, 'NotFoundError')) {
    return jsonResponse({ error: 'not found' }, 404);
  }
  if (isNamed(err, 'QuotaExceededError')) {
    const e = err as { quota: string; limit: number; used: number; scope?: string };
    return jsonResponse({ error: 'quota exceeded', quota: e.quota, limit: e.limit, used: e.used, scope: e.scope }, 429);
  }
  const status = err instanceof PilotsError ? err.status : 0;
  log(`fleet call failed (upstream status ${status}): ${(err as Error)?.message ?? 'unknown'}`);
  return jsonResponse({ error: 'fleet unavailable', upstream_status: status }, 502);
}

function isNamed(err: unknown, name: string): boolean {
  return typeof err === 'object' && err !== null && (err as { name?: string }).name === name;
}

function jsonResponse(body: unknown, status: number): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json; charset=utf-8' },
  });
}
