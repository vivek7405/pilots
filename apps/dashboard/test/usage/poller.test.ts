/**
 * The usage poller.
 *
 * Three properties are load-bearing. A dead host is skipped. A live host that
 * FAILS does not advance its watermark, so the next tick re-asks the same
 * window rather than losing it. And a re-delivered window upserts rather than
 * duplicating, which is what makes that retry safe.
 *
 * Counterfactuals: drop the `alive` check and the dead host gets fetched; move
 * the watermark write outside the try and the failing host's watermark
 * advances; drop `onConflictDoUpdate` and the re-delivery test throws on the
 * unique index.
 */

import assert from 'node:assert/strict';
import { after, before, beforeEach, test } from 'node:test';
import { bootApp } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';
import type { Host, UsageResponse } from '@pilots/sdk';
import type { HostUsageQuery } from '#modules/usage/host-fetch.server.ts';

let app: TestApp;
let poller: typeof import('#modules/usage/poller.server.ts');
let db: typeof import('#db/connection.server.ts')['db'];

const HOUR = 3_600_000;

function host(id: string, alive: boolean, ip = `192.0.2.${id.length}`): Host {
  return { id, public_ip: ip, cpu_free: 4, mem_free_mib: 1024, last_seen: 0, alive } as Host;
}

/** A fetcher that records what it was asked and answers from a script. */
function fetcher(script: Record<string, UsageResponse | Error>) {
  const asked: HostUsageQuery[] = [];
  const fn = async (query: HostUsageQuery): Promise<UsageResponse> => {
    asked.push(query);
    const answer = script[query.ip];
    if (!answer) throw new Error(`no script for ${query.ip}`);
    if (answer instanceof Error) throw answer;
    return answer;
  };
  return { asked, fn };
}

function usage(hostId: string, since: number, until: number, orgs: Record<string, number>): UsageResponse {
  return {
    host_id: hostId,
    since: Math.floor(since / 1000),
    until: Math.floor(until / 1000),
    orgs: Object.fromEntries(
      Object.entries(orgs).map(([org, seconds]) => [
        org,
        { machine_seconds: seconds, vcpu_seconds: seconds * 2, mib_seconds: seconds * 512, volume_gib_seconds: seconds },
      ]),
    ),
  };
}

before(async () => {
  app = await bootApp();
  poller = await import('#modules/usage/poller.server.ts');
  ({ db } = await import('#db/connection.server.ts'));
});

beforeEach(async () => {
  app.fleet.data.hosts.length = 0;
  app.fleet.calls.length = 0;
  await db.delete((await import('#db/schema.server.ts')).usageSamples);
});

after(() => {
  poller.stopUsagePoller();
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

test('a dead host is never asked, and a live one is upserted per org', async () => {
  app.fleet.data.hosts.push(host('h-live', true, '192.0.2.10'), host('h-dead', false, '192.0.2.11'));
  const now = Date.now();
  const f = fetcher({ '192.0.2.10': usage('h-live', now - HOUR, now, { 'org-a': 60, 'org-b': 30 }) });

  await poller.tick({ fetchUsage: f.fn });

  assert.deepEqual(f.asked.map((q) => q.ip), ['192.0.2.10'], 'a host the fleet reports as down is skipped');

  const rows = await db.query.usageSamples.findMany();
  assert.equal(rows.length, 2, 'one row per org in the answer');
  assert.deepEqual(rows.map((r) => r.orgId).sort(), ['org-a', 'org-b']);
  const a = rows.find((r) => r.orgId === 'org-a')!;
  assert.equal(a.machineSeconds, 60);
  assert.equal(a.vcpuSeconds, 120);
  assert.equal(a.hostId, 'h-live');
});

test('TLS is verified against the API hostname while the socket goes to the IP', async () => {
  app.fleet.data.hosts.push(host('h1', true, '192.0.2.20'));
  const now = Date.now();
  const f = fetcher({ '192.0.2.20': usage('h1', now - HOUR, now, {}) });

  await poller.tick({ fetchUsage: f.fn });

  assert.equal(f.asked[0].ip, '192.0.2.20', 'the connection is made to the host address');
  assert.equal(
    f.asked[0].apiHost,
    'api.pilots.test',
    'and the certificate is checked against the name the fleet serves, not the IP',
  );
});

test('the watermark advances for a host that answered, and only for that host', async () => {
  const now = Date.now();
  app.fleet.data.hosts.push(host('h-ok', true, '192.0.2.30'), host('h-bad', true, '192.0.2.31'));

  const first = fetcher({
    '192.0.2.30': usage('h-ok', now - HOUR, now, { 'org-a': 10 }),
    '192.0.2.31': new Error('connection refused'),
  });
  await poller.tick({ fetchUsage: first.fn });

  const okWatermark = await poller.watermarkFor('h-ok');
  const badWatermark = await poller.watermarkFor('h-bad', now);
  // The ledger's window is in whole seconds, so the watermark lands on the
  // second boundary rather than the exact millisecond the tick started.
  assert.ok(
    okWatermark <= now && okWatermark > now - 1000,
    "the answering host's watermark moved to the window it reported",
  );
  assert.ok(
    badWatermark < now - HOUR * 20,
    'the failing host stayed at its first-look default, so nothing was lost',
  );

  // A second tick asks the failing host from the SAME point.
  const second = fetcher({
    '192.0.2.30': usage('h-ok', now, now + HOUR, { 'org-a': 5 }),
    '192.0.2.31': new Error('still down'),
  });
  await poller.tick({ fetchUsage: second.fn });

  const askedBad = second.asked.find((q) => q.ip === '192.0.2.31')!;
  assert.equal(askedBad.since, first.asked.find((q) => q.ip === '192.0.2.31')!.since);
});

test('a re-delivered window updates its row rather than duplicating it', async () => {
  const now = Date.now();
  app.fleet.data.hosts.push(host('h1', true, '192.0.2.40'));
  const window = usage('h1', now - HOUR, now, { 'org-a': 10 });

  await poller.tick({ fetchUsage: fetcher({ '192.0.2.40': window }).fn });
  // The same window again, with a larger cumulative total.
  const revised = usage('h1', now - HOUR, now, { 'org-a': 25 });
  await poller.tick({ fetchUsage: fetcher({ '192.0.2.40': revised }).fn });

  const rows = await db.query.usageSamples.findMany();
  assert.equal(rows.length, 1, 'still one row for that (host, org, window)');
  assert.equal(rows[0].machineSeconds, 25, 'and it carries the newer total');
});

test('a host with no public address is skipped rather than dialled by id', async () => {
  app.fleet.data.hosts.push({ id: 'h-noip', cpu_free: 1, mem_free_mib: 1, last_seen: 0, alive: true } as Host);
  const f = fetcher({});
  await poller.tick({ fetchUsage: f.fn });
  assert.equal(f.asked.length, 0);
});
