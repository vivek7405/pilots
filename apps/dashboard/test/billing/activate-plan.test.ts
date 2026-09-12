/**
 * Activating a plan, and the fleet write that is the whole point of it.
 *
 * A plan stored here and never sent to the fleet would be a number on a page
 * that nothing enforces, which is the same class of mistake as an expiry
 * nobody reads. So the assertions are about the QUOTA the fake fleet received,
 * field by field, not about the fact a call happened: a bundle mapped onto the
 * wrong wire name arrives as zero, and zero is a frozen team rather than a
 * missing feature.
 *
 * Counterfactuals: drop the `quotas.put` call and the first test fails; swap
 * two fields in `quotaWire` and the field-by-field assertion fails; write the
 * local row before the fleet call and the "fleet refused" test finds a team on
 * a plan it is not being held to; drop the personal-team guard and a personal
 * team gets a billing account.
 */

import assert from 'node:assert/strict';
import { after, before, beforeEach, test } from 'node:test';
import { invokeActionForTest } from '@webjsdev/server/testing';
import { APP_DIR, bootApp, signInAs } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';
import { PLANS } from '#modules/billing/plans.ts';

let app: TestApp;
let handler: { handle: TestApp['handle']; appDir: string };
let ownerCookie = '';
let personalCookie = '';
let orgId = '';
let personalId = '';
let db: (typeof import('#db/connection.server.ts'))['db'];

const PLAN = 'modules/billing/actions/activate-plan.server.ts';

interface Envelope {
  success?: boolean;
  error?: string;
  status?: number;
  redirect?: string;
}

function form(fields: Record<string, string>): FormData {
  const fd = new FormData();
  for (const [k, v] of Object.entries(fields)) fd.set(k, v);
  return fd;
}

before(async () => {
  app = await bootApp();
  handler = { handle: app.handle, appDir: APP_DIR };
  const cookie = await signInAs(app.handle, { id: 940, login: 'payer' });

  ({ db } = await import('#db/connection.server.ts'));
  const schema = await import('#db/schema.server.ts');
  const user = (await db.query.users.findMany()).find((u) => u.login === 'payer')!;
  personalId = (await db.query.orgs.findMany()).find((o) => o.personal && o.ownerId === user.id)!.id;

  const [org] = await db
    .insert(schema.orgs)
    .values({ slug: 'payco', name: 'Payco', personal: false, ownerId: user.id })
    .returning();
  orgId = org.id;
  await db.insert(schema.memberships).values({ userId: user.id, orgId, role: 'owner' });

  ownerCookie = `${cookie}; pilots_org=${orgId}`;
  personalCookie = `${cookie}; pilots_org=${personalId}`;
});

beforeEach(() => {
  app.fleet.calls.length = 0;
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

test('activating pro writes the plan and pushes its bundle to the fleet as the quota', async () => {
  const result = (await invokeActionForTest(handler, PLAN, 'activatePlan', [form({ plan: 'pro' })], {
    extraCookies: ownerCookie,
  })) as Envelope;

  assert.equal(result.success, true);
  assert.equal(result.redirect, '/org?ok=plan-changed');

  const call = app.fleet.calls.find((c) => c.method === 'quotas.put');
  assert.ok(call, 'a plan nothing sends to the fleet is a number nothing enforces');
  assert.equal(call.args[0], orgId, 'and it is written for the team the visitor is acting as');
  assert.deepEqual(call.args[1], {
    org_id: orgId,
    max_machines: PLANS.pro.quota.instances,
    max_vcpus: PLANS.pro.quota.vcpus,
    max_mem_mib: PLANS.pro.quota.memMib,
    max_volume_gib: PLANS.pro.quota.storageGib,
    max_builds: PLANS.pro.quota.builds,
  });

  const org = (await db.query.orgs.findMany()).find((o) => o.id === orgId)!;
  assert.ok(org.billingAccountId, 'and the team now has an account');
  const account = (await db.query.billingAccounts.findMany()).find((a) => a.id === org.billingAccountId)!;
  assert.equal(account.plan, 'pro');
  assert.equal(account.status, 'active');
  assert.equal(account.provider, 'none', 'there is exactly one provider and it collects nothing');
  assert.equal(account.providerRef, null);
});

test('switching back to free reuses the same account rather than making a second', async () => {
  const before = (await db.query.billingAccounts.findMany()).length;
  const result = (await invokeActionForTest(handler, PLAN, 'activatePlan', [form({ plan: 'free' })], {
    extraCookies: ownerCookie,
  })) as Envelope;

  assert.equal(result.success, true);
  assert.equal((await db.query.billingAccounts.findMany()).length, before);

  const org = (await db.query.orgs.findMany()).find((o) => o.id === orgId)!;
  const account = (await db.query.billingAccounts.findMany()).find((a) => a.id === org.billingAccountId)!;
  assert.equal(account.plan, 'free');

  const call = app.fleet.calls.find((c) => c.method === 'quotas.put')!;
  assert.equal(
    (call.args[1] as { max_machines: number }).max_machines,
    PLANS.free.quota.instances,
    'the ceilings come down with the plan, or downgrading buys nothing',
  );
});

test('a fleet that refuses the quota leaves the stored plan alone', async () => {
  const org = (await db.query.orgs.findMany()).find((o) => o.id === orgId)!;
  const before = (await db.query.billingAccounts.findMany()).find((a) => a.id === org.billingAccountId)!;

  app.fleet.data.quotaPutError = new Error('nope');
  const result = (await invokeActionForTest(handler, PLAN, 'activatePlan', [form({ plan: 'pro' })], {
    extraCookies: ownerCookie,
  })) as Envelope;
  app.fleet.data.quotaPutError = null;

  assert.equal(result.success, false);
  assert.equal(result.status, 502);
  const after = (await db.query.billingAccounts.findMany()).find((a) => a.id === org.billingAccountId)!;
  assert.equal(after.plan, before.plan, 'a team must never show a plan it is not being held to');
});

test('a personal team stays free and never gets an account', async () => {
  const result = (await invokeActionForTest(handler, PLAN, 'activatePlan', [form({ plan: 'pro' })], {
    extraCookies: personalCookie,
  })) as Envelope;

  assert.equal(result.success, false);
  assert.equal(result.status, 422);
  assert.equal(
    app.fleet.calls.some((c) => c.method === 'quotas.put'),
    false,
    'and nothing was pushed to the fleet on the way to the refusal',
  );
  assert.equal((await db.query.orgs.findMany()).find((o) => o.id === personalId)!.billingAccountId, null);
});

test('a plan nobody defined is a field error', async () => {
  const result = (await invokeActionForTest(handler, PLAN, 'activatePlan', [form({ plan: 'enterprise' })], {
    extraCookies: ownerCookie,
  })) as Envelope;

  assert.equal(result.success, false);
  assert.equal(app.fleet.calls.some((c) => c.method === 'quotas.put'), false);
});

test('the payment provider makes no network call', async () => {
  const { NoneProvider, paymentProvider } = await import('#modules/billing/provider.server.ts');
  assert.equal(paymentProvider(), NoneProvider, 'exactly one implementation');

  const realFetch = globalThis.fetch;
  let reached = false;
  globalThis.fetch = (async () => {
    reached = true;
    return new Response('{}');
  }) as typeof globalThis.fetch;
  try {
    const org = (await db.query.orgs.findMany()).find((o) => o.id === orgId)!;
    assert.equal(await NoneProvider.setup(org), null, 'nowhere to send the visitor');
    assert.equal((await NoneProvider.status(null)).state, 'none');
  } finally {
    globalThis.fetch = realFetch;
  }
  assert.equal(reached, false, 'self-hosting must not require a billing integration to be reachable');
});
