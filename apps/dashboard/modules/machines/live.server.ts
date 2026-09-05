/**
 * The live machine feed, delivered PER ORG.
 *
 * `broadcast(path, data)` fans a message out to every socket connected to that
 * path, so one `broadcast('/api/machines', ...)` would hand org A's machines to
 * every viewer of org B. That is why this module keeps its own subscriber map
 * instead: one entry per org, and a tick sends each org only its own rows.
 *
 * The map lives on `globalThis` because in dev the route module is re-imported
 * per connection, so a module-scope `Map` would be a fresh empty map for every
 * socket and the shared tick would drive nobody.
 *
 * One interval serves every subscriber: it starts on the first socket and is
 * cleared on the last, so an idle dashboard makes no fleet calls at all.
 */

import { listMachines } from '#modules/fleet/client.server.ts';
import type { Machine } from '@pilots/sdk';

/** The socket surface this module needs; the framework's `ws` satisfies it. */
export interface LiveSocket {
  send(data: string): void;
  close(code?: number, reason?: string): void;
}

export interface Snapshot {
  type: 'snapshot';
  machines: Machine[];
}

export interface Delta {
  type: 'delta';
  upsert: Machine[];
  remove: string[];
}

interface LiveState {
  orgs: Map<string, Set<LiveSocket>>;
  last: Map<string, Map<string, string>>;
  timer: ReturnType<typeof setInterval> | null;
}

const TICK_MS = 2_000;

function state(): LiveState {
  const g = globalThis as { __pilots_live?: LiveState };
  return (g.__pilots_live ??= { orgs: new Map(), last: new Map(), timer: null });
}

/**
 * The fields a viewer can SEE change. A machine whose `last_activity` moved but
 * whose state, host, URL, name and last start did not is not a change anyone is
 * looking at, and sending it every two seconds would make the delta a full list
 * forever.
 *
 * `last_start` is in here because a cold boot changes none of the others: the
 * machine goes suspended and comes back running on some host with the same URL,
 * which is exactly what a resume looks like. Without it the one visible sign
 * that a guest lost its memory would never reach an open dashboard.
 */
function fingerprint(m: Machine): string {
  // NUL as the separator: a machine name may contain a space, and none of
  // these fields can contain a NUL. It is written as the escape so the file
  // stays text. A raw 0x00 here made git classify the file as binary, which
  // hid every change to it from review, and anything that assumed text could
  // drop the byte and quietly turn this into join('').
  return [m.state, m.host_id ?? '', m.url ?? '', m.name ?? '', m.last_start ?? ''].join('\u0000');
}

/**
 * Registers a socket for an org and sends it the opening snapshot.
 *
 * `last` is per-org state shared by every viewer of that org, and only
 * `tick()` may advance it. A subscribe that overwrote it with a fresh read
 * would swallow a pending delta: a machine changes, a second tab opens before
 * the next tick and re-reads the fleet, the map now already holds the new
 * fingerprint, and the tick finds nothing to send, so the first tab shows the
 * old state until something else changes. So the map is seeded only when the
 * org has no entry yet, and the joining socket's snapshot leaves it alone.
 */
export async function subscribe(orgId: string, ws: LiveSocket): Promise<void> {
  const s = state();
  const set = s.orgs.get(orgId) ?? new Set<LiveSocket>();
  set.add(ws);
  s.orgs.set(orgId, set);

  const machines = await listMachines(orgId);
  if (!s.last.has(orgId)) s.last.set(orgId, new Map(machines.map((m) => [m.id, fingerprint(m)])));
  ws.send(JSON.stringify({ type: 'snapshot', machines } satisfies Snapshot));

  if (!s.timer) {
    s.timer = setInterval(() => {
      void tick();
    }, TICK_MS);
    // The interval must never be the reason a process stays alive.
    s.timer.unref?.();
  }
}

/** Removes a socket, stopping the shared tick when it was the last one. */
export function unsubscribe(orgId: string, ws: LiveSocket): void {
  const s = state();
  const set = s.orgs.get(orgId);
  if (!set) return;
  set.delete(ws);
  if (set.size === 0) {
    s.orgs.delete(orgId);
    s.last.delete(orgId);
  }
  if (s.orgs.size === 0 && s.timer) {
    clearInterval(s.timer);
    s.timer = null;
  }
}

/**
 * One pass over every subscribed org. Exported so a test can drive it without
 * waiting two seconds of wall clock.
 */
export async function tick(): Promise<void> {
  const s = state();
  for (const [orgId, sockets] of s.orgs) {
    if (sockets.size === 0) continue;
    let machines: Machine[];
    try {
      machines = await listMachines(orgId);
    } catch (err) {
      // A fleet blip must not tear down every viewer's socket; the next tick
      // re-asks and the client keeps the rows it already has.
      console.error(`live machines: list failed for org ${orgId}: ${(err as Error).message}`);
      continue;
    }

    const previous = s.last.get(orgId) ?? new Map<string, string>();
    const current = new Map(machines.map((m) => [m.id, fingerprint(m)]));
    const upsert = machines.filter((m) => previous.get(m.id) !== current.get(m.id));
    const remove = [...previous.keys()].filter((id) => !current.has(id));
    s.last.set(orgId, current);
    if (upsert.length === 0 && remove.length === 0) continue;

    const message = JSON.stringify({ type: 'delta', upsert, remove } satisfies Delta);
    for (const ws of sockets) ws.send(message);
  }
}

/** Test-only: how many sockets are registered for an org right now. */
export function subscriberCount(orgId: string): number {
  return state().orgs.get(orgId)?.size ?? 0;
}
