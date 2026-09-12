/**
 * What the team page offers, to whom.
 *
 * The action is the control and the page is the courtesy: every refusal here
 * is also asserted against the action in `roles.test.ts` and
 * `workspaces.test.ts`. What this file pins is that the page does not OFFER a
 * control the visitor cannot use, because a button that always answers 403 is
 * a product that lies about what the reader is allowed to do.
 *
 * Counterfactual: render the danger section for a personal team and the page
 * offers to delete an account's own home, which nothing can then restore.
 */

import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { asUser, bootApp, signInAs } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';

let app: TestApp;
let personalCookie = '';
let memberCookie = '';
let ownerCookie = '';

async function page(cookie: string): Promise<string> {
  const res = await app.handle(new Request('http://localhost/org', asUser(cookie)));
  assert.equal(res.status, 200);
  return res.text();
}

before(async () => {
  app = await bootApp();
  const owner = await signInAs(app.handle, { id: 960, login: 'pageowner' });
  const mate = await signInAs(app.handle, { id: 961, login: 'pagemate' });

  const { db } = await import('#db/connection.server.ts');
  const schema = await import('#db/schema.server.ts');
  const users = await db.query.users.findMany();
  const ownerId = users.find((u) => u.login === 'pageowner')!.id;
  const mateId = users.find((u) => u.login === 'pagemate')!.id;

  const [org] = await db
    .insert(schema.orgs)
    .values({ slug: 'pageco', name: 'Pageco', personal: false, ownerId })
    .returning();
  await db.insert(schema.memberships).values({ userId: ownerId, orgId: org.id, role: 'owner' });
  await db.insert(schema.memberships).values({ userId: mateId, orgId: org.id, role: 'member' });

  personalCookie = owner;
  ownerCookie = `${owner}; pilots_org=${org.id}`;
  memberCookie = `${mate}; pilots_org=${org.id}`;
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

test('an owner of a shared team is offered everything', async () => {
  const body = await page(ownerCookie);
  assert.match(body, /Rename/);
  assert.match(body, /Hand over/);
  assert.match(body, /Leave this team/);
  assert.match(body, /Delete team/);
  assert.match(body, /Switch to Pro/, 'the plan is the owner’s to change');
  assert.match(body, /Builders/);
  assert.match(body, /Add a member/);
});

test('a member is offered only the one thing a member may do', async () => {
  const body = await page(memberCookie);
  assert.match(body, /Leave this team/, 'leaving is theirs');
  assert.doesNotMatch(body, /Delete team/);
  assert.doesNotMatch(body, /Hand over/);
  assert.doesNotMatch(body, /Switch to Pro/);
  assert.doesNotMatch(body, /Add a member/, 'a member changes nothing about the team');
});

test('a personal team is offered neither leaving nor deleting', async () => {
  const body = await page(personalCookie);
  assert.doesNotMatch(body, /Leave this team/, 'an account with no team can see nothing');
  assert.doesNotMatch(body, /Delete team/);
  assert.doesNotMatch(body, /Hand over/);
  assert.doesNotMatch(body, /Switch to Pro/, 'a personal team is always on the free plan');
  assert.match(body, /always on the free plan/, 'and the page says so rather than leaving it unexplained');
  assert.match(body, /Create a team/, 'with the way to get one that is not');
});
