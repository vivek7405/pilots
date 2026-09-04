/**
 * The usage poller: ask every live host for its ledger, upsert what it says.
 *
 * Usage is measured on the hosts and only aggregated here. That ordering is
 * what keeps the dashboard out of every request path: a host that cannot reach
 * this app keeps metering, and this app catches up on the next tick.
 *
 * It lives here rather than in a scheduler because the framework ships none. An
 * app-root `instrumentation.ts` `register()` runs exactly once per process at
 * boot, before the route table is built, so an interval started from there is
 * the sanctioned home for recurring work.
 *
 * It survives a redeploy with nothing held in memory: the watermark is
 * `MAX(window_end)` per host in SQLite on the volume, and each tick computes
 * its own wall-clock window. A restore from a release snapshot resumes with a
 * fresh clock rather than an accumulated one.
 */

import { desc, eq } from 'drizzle-orm';
import { broadcast } from '@webjsdev/server';
import { db } from '#db/connection.server.ts';
import { usageSamples } from '#db/schema.server.ts';
import { apiHost, fleet } from '#modules/fleet/client.server.ts';
import { fetchHostUsage } from './host-fetch.server.ts';
import type { HostUsageQuery } from './host-fetch.server.ts';
import type { UsageResponse } from '@pilots/sdk';

/** How far back a host is asked on first sight. */
const FIRST_LOOK_MS = 24 * 60 * 60 * 1000;
const DEFAULT_INTERVAL_MS = 60_000;

export type HostFetcher = (query: HostUsageQuery) => Promise<UsageResponse>;

export interface PollerOptions {
  every?: number;
  /** The seam a test dials through; production uses the real HTTPS call. */
  fetchUsage?: HostFetcher;
}

interface PollerGlobal {
  __pilots_usage_poller?: ReturnType<typeof setInterval>;
}

/** Starts the interval once per process, and runs one tick immediately. */
export function startUsagePoller(opts: PollerOptions = {}): void {
  const g = globalThis as PollerGlobal;
  if (g.__pilots_usage_poller) return;

  const every = opts.every ?? DEFAULT_INTERVAL_MS;
  g.__pilots_usage_poller = setInterval(() => {
    void tick(opts).catch((err: Error) => console.error(`usage poller: ${err.message}`));
  }, every);
  // Never the reason a process stays alive.
  g.__pilots_usage_poller.unref?.();

  void tick(opts).catch((err: Error) => console.error(`usage poller: ${err.message}`));
}

/** Stops the interval. Used by tests and by a clean shutdown. */
export function stopUsagePoller(): void {
  const g = globalThis as PollerGlobal;
  if (!g.__pilots_usage_poller) return;
  clearInterval(g.__pilots_usage_poller);
  delete g.__pilots_usage_poller;
}

/** One pass over the fleet. Exported so a test drives it without a clock. */
export async function tick(opts: PollerOptions = {}): Promise<void> {
  const fetchUsage = opts.fetchUsage ?? fetchHostUsage;
  const apiKey = process.env.PILOT_ADMIN_KEY ?? '';

  const hosts = await fleet.hosts.list();
  // Every signed-in viewer is entitled to the same host inventory, so this is
  // the one path where broadcast's fan-out-to-everyone is what we want.
  broadcast('/api/hosts', JSON.stringify({ hosts }));

  const until = Date.now();
  for (const host of hosts) {
    if (!host.alive || !host.public_ip) continue;
    try {
      const since = await watermark(host.id, until);
      const usage = await fetchUsage({
        ip: host.public_ip,
        apiHost,
        apiKey,
        since: Math.floor(since / 1000),
        until: Math.floor(until / 1000),
      });
      await record(host.id, usage, since, until);
    } catch (err) {
      // The watermark does NOT advance for a host that failed, so the next
      // tick re-asks from the same point. The ledger is cumulative per window
      // and the upsert is keyed on it, so re-asking is free and idempotent.
      console.error(`usage poller: host ${host.id}: ${(err as Error).message}`);
    }
  }
}

/** Where this host was last read to, or 24h ago the first time it is seen. */
async function watermark(hostId: string, now: number): Promise<number> {
  // The newest row rather than a MAX() projection: drizzle rc.3 removed the
  // projection form of select(), and this reads the (host_id, window_end)
  // index the schema declares for exactly this query.
  const row = await db
    .select()
    .from(usageSamples)
    .where(eq(usageSamples.hostId, hostId))
    .orderBy(desc(usageSamples.windowEnd))
    .limit(1)
    .get();
  return row ? row.windowEnd.getTime() : now - FIRST_LOOK_MS;
}

/** One upsert per org in the host's answer. */
async function record(hostId: string, usage: UsageResponse, since: number, until: number): Promise<void> {
  // The host's own window wins when it reports one: it knows what it actually
  // measured, and the request's `since` is only a hint.
  const windowStart = new Date(usage.since ? usage.since * 1000 : since);
  const windowEnd = new Date(usage.until ? usage.until * 1000 : until);

  for (const [orgId, totals] of Object.entries(usage.orgs ?? {})) {
    await db
      .insert(usageSamples)
      .values({
        orgId,
        hostId,
        windowStart,
        windowEnd,
        machineSeconds: Math.round(totals.machine_seconds ?? 0),
        vcpuSeconds: Math.round(totals.vcpu_seconds ?? 0),
        mibSeconds: Math.round(totals.mib_seconds ?? 0),
        volumeGibSeconds: Math.round(totals.volume_gib_seconds ?? 0),
      })
      .onConflictDoUpdate({
        target: [usageSamples.hostId, usageSamples.orgId, usageSamples.windowStart],
        set: {
          windowEnd,
          machineSeconds: Math.round(totals.machine_seconds ?? 0),
          vcpuSeconds: Math.round(totals.vcpu_seconds ?? 0),
          mibSeconds: Math.round(totals.mib_seconds ?? 0),
          volumeGibSeconds: Math.round(totals.volume_gib_seconds ?? 0),
        },
      });
  }
}

/** Test helper: the watermark this host would be asked from next. */
export function watermarkFor(hostId: string, now = Date.now()): Promise<number> {
  return watermark(hostId, now);
}
