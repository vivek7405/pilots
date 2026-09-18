/**
 * The billing hooks: usage out as CSV and JSON.
 *
 * "Billing hooks" is the export and nothing more. There is no price, no
 * invoice and no payment provider here, so what these assert is that a finance
 * system can load the metered quantities and that one org cannot read
 * another's.
 *
 * Counterfactuals: drop the membership check and "an org the caller does not
 * belong to is a 404" fails; sum the totals from a different field and the
 * JSON totals stop matching the rows.
 */

import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { bootApp, signInAs, asUser } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';

let app: TestApp;
let cookie = '';
let orgId = '';

before(async () => {
  app = await bootApp();
  cookie = await signInAs(app.handle, { id: 900, login: 'finance' });

  const { db } = await import('#db/connection.server.ts');
  const schema = await import('#db/schema.server.ts');
  orgId = (await db.query.orgs.findMany()).find((o) => o.slug === 'finance')!.id;

  const day = (n: number) => new Date(Date.UTC(2026, 8, n));
  await db.insert(schema.usageSamples).values([
    {
      orgId,
      hostId: 'h1',
      windowStart: day(2),
      windowEnd: day(3),
      machineSeconds: 100,
      vcpuSeconds: 200,
      mibSeconds: 300,
      volumeGibSeconds: 400,
    },
    {
      orgId,
      hostId: 'h2',
      windowStart: day(3),
      windowEnd: day(4),
      machineSeconds: 10,
      vcpuSeconds: 20,
      mibSeconds: 30,
      volumeGibSeconds: 40,
    },
    // Another org's row, in the same period, on the same hosts.
    {
      orgId: 'somebody-else',
      hostId: 'h1',
      windowStart: day(2),
      windowEnd: day(3),
      machineSeconds: 9999,
      vcpuSeconds: 9999,
      mibSeconds: 9999,
      volumeGibSeconds: 9999,
    },
  ]);
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

const PERIOD = 'since=2026-09-01&until=2026-10-01';

test('the CSV has the exact header row, one line per sample, and a filename', async () => {
  const res = await app.handle(
    new Request(`http://localhost/api/usage?${PERIOD}&format=csv`, asUser(cookie)),
  );
  assert.equal(res.status, 200);
  assert.match(res.headers.get('content-type')!, /^text\/csv/);
  assert.equal(
    res.headers.get('content-disposition'),
    'attachment; filename="usage-finance-2026-09-01-2026-10-01.csv"',
  );

  const lines = (await res.text()).trim().split('\n');
  assert.equal(
    lines[0],
    'org_id,host_id,window_start,window_end,machine_seconds,vcpu_seconds,mib_seconds,volume_gib_seconds',
  );
  assert.equal(lines.length, 3, 'a header and this org\'s two samples, not the third org\'s');
  assert.equal(lines.some((l) => l.includes('9999')), false, "another org's numbers are not in the file");
});

test('the JSON totals equal the sum of the rows', async () => {
  const res = await app.handle(new Request(`http://localhost/api/usage?${PERIOD}`, asUser(cookie)));
  assert.equal(res.status, 200);

  const body = (await res.json()) as {
    samples: { machine_seconds: number; vcpu_seconds: number }[];
    totals: { machine_seconds: number; vcpu_seconds: number; mib_seconds: number; volume_gib_seconds: number };
  };
  assert.equal(body.samples.length, 2);
  assert.equal(body.totals.machine_seconds, 110);
  assert.equal(body.totals.vcpu_seconds, 220);
  assert.equal(body.totals.mib_seconds, 330);
  assert.equal(body.totals.volume_gib_seconds, 440);
  assert.equal(
    body.totals.machine_seconds,
    body.samples.reduce((sum, s) => sum + s.machine_seconds, 0),
  );
});

test('an org the caller does not belong to is a 404', async () => {
  const res = await app.handle(
    new Request(`http://localhost/api/usage?org=somebody-else&${PERIOD}`, asUser(cookie)),
  );
  assert.equal(res.status, 404, 'a 403 would confirm the org exists');
});

test('a period outside the samples returns an empty export, not an error', async () => {
  const res = await app.handle(
    new Request('http://localhost/api/usage?since=2025-01-01&until=2025-02-01', asUser(cookie)),
  );
  assert.equal(res.status, 200);
  const body = (await res.json()) as { samples: unknown[]; totals: { machine_seconds: number } };
  assert.deepEqual(body.samples, []);
  assert.equal(body.totals.machine_seconds, 0);
});

test('signed out is a 401', async () => {
  const res = await app.handle(new Request(`http://localhost/api/usage?${PERIOD}`));
  assert.equal(res.status, 401);
});
