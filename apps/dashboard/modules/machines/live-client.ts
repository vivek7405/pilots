/**
 * One socket to the live machine feed, shared by every element on the page.
 *
 * The server side already fans one 2s tick out to every subscriber of an org
 * (`live.server.ts`); this is the same shape on the client. A page can carry a
 * status element per app card and per service card, and a socket each would
 * mean a dozen connections, a dozen opening snapshots and a dozen copies of
 * the same rows -- for one feed that says the same thing to all of them.
 *
 * So the rows live here, the socket opens on the first subscriber and closes
 * on the last, and each listener is handed the whole current set. Applying a
 * delta once and re-rendering from the result is also what keeps two elements
 * from disagreeing: they read the same array.
 *
 * `<machine-list>` keeps its own connection on purpose. It owns its rows, its
 * filter and its pagination, and it is the only live element on the page it
 * appears on, so there is nothing to share with.
 */

import { connectWS } from '@webjsdev/core';
import type { Machine } from '#modules/machines/types.ts';

export interface Snapshot {
  type: 'snapshot';
  machines: Machine[];
}

export interface Delta {
  type: 'delta';
  upsert: Machine[];
  remove: string[];
}

export type Listener = (machines: Machine[]) => void;

interface Feed {
  listeners: Set<Listener>;
  rows: Machine[];
  /** Whether the opening snapshot has landed; before it, `rows` is empty. */
  seeded: boolean;
  conn: { close(): void } | null;
}

/**
 * Module scope would be enough in a browser, where a module is instantiated
 * once per page load. It hangs off `globalThis` because the dev server can
 * re-serve a module, and a second copy would open a second socket and leave
 * the first one's listeners on a feed nothing drives.
 */
function feed(): Feed {
  const g = globalThis as { __pilots_machine_feed?: Feed };
  return (g.__pilots_machine_feed ??= { listeners: new Set(), rows: [], seeded: false, conn: null });
}

function apply(f: Feed, message: Snapshot | Delta): boolean {
  if (message?.type === 'snapshot') {
    f.rows = message.machines ?? [];
    f.seeded = true;
    return true;
  }
  if (message?.type !== 'delta') return false;

  const removed = new Set(message.remove ?? []);
  const next = f.rows.filter((m) => !removed.has(m.id));
  for (const row of message.upsert ?? []) {
    const at = next.findIndex((m) => m.id === row.id);
    if (at >= 0) next[at] = row;
    else next.push(row);
  }
  f.rows = next;
  return true;
}

/**
 * Registers a listener and returns its unsubscribe.
 *
 * A listener that joins after the snapshot is called immediately with the rows
 * already held, so an element added late renders live state rather than
 * waiting for the next machine to change. One that joins first hears nothing
 * until the snapshot lands, and keeps showing what the server rendered.
 */
export function subscribeMachines(listener: Listener): () => void {
  const f = feed();
  f.listeners.add(listener);
  if (f.seeded) listener(f.rows);

  if (!f.conn) {
    f.conn = connectWS('/api/machines', {
      onMessage: (message: Snapshot | Delta) => {
        const g = feed();
        if (!apply(g, message)) return;
        for (const l of g.listeners) l(g.rows);
      },
      onClose: () => {
        // The rows stay: a dropped socket is not news that every machine is
        // gone, and the framework reconnects. `seeded` stays true so a late
        // element still gets what was last known rather than nothing.
        feed().conn = null;
      },
    });
  }

  return () => {
    const g = feed();
    g.listeners.delete(listener);
    if (g.listeners.size > 0) return;
    g.conn?.close();
    g.conn = null;
    // Dropped with the last listener, so the next page to open this feed
    // starts from its own server render rather than from rows that have been
    // sitting unwatched for however long.
    g.rows = [];
    g.seeded = false;
  };
}

/** Test-only: how many elements are listening right now. */
export function listenerCount(): number {
  return feed().listeners.size;
}

/** Test-only: drive a message through the feed without a socket. */
export function deliver(message: Snapshot | Delta): void {
  const f = feed();
  if (!apply(f, message)) return;
  for (const l of f.listeners) l(f.rows);
}
