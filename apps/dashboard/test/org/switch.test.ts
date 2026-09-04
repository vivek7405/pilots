/**
 * Switching the acting org, and where the switch sends the visitor afterwards.
 *
 * `back` is a form field, so it is attacker-controlled on a page that also
 * carries the session cookie. The action must send the visitor to a path on
 * this origin or to the default, and nowhere else. The backslash case is the
 * one a naive check misses: browsers normalise `/\host` to `//host` when they
 * resolve a Location, so it is protocol-relative in effect.
 *
 * The action is driven the way the layout drives it: a bound `<form>` posted
 * back with scripting off, so the returned `Response` (a cookie has to ride a
 * header) is the one a browser would see. The switcher only renders for a
 * visitor with more than one org, so the fixture gives them two.
 *
 * Counterfactual: replace `localPath(...)` with `back.startsWith('/') &&
 * !back.startsWith('//') ? back : '/machines'` and the backslash case fails
 * with `Location: /\evil.com`.
 */

import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { getSetCookies, submitForm } from '@webjsdev/server/testing';
import { bootApp, signInAs } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';

let app: TestApp;
let cookie = '';
let orgId = '';

/** Post the org switcher from the layout of `/machines`, with these fields. */
async function switchTo(fields: Record<string, string>): Promise<Response> {
  return submitForm(app.handle, '/machines', fields, { cookies: cookie, match: /name="org"/ });
}

before(async () => {
  app = await bootApp();
  cookie = await signInAs(app.handle, { id: 700, login: 'switcher' });
  const { db } = await import('#db/connection.server.ts');
  const schema = await import('#db/schema.server.ts');
  const user = (await db.query.users.findMany()).find((u) => u.login === 'switcher')!;
  const [second] = await db
    .insert(schema.orgs)
    .values({ slug: 'second', name: 'second', personal: false, ownerId: user.id })
    .returning();
  await db.insert(schema.memberships).values({ userId: user.id, orgId: second.id, role: 'member' });
  orgId = second.id;
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

test('a same-origin path is honoured, and the org cookie rides the redirect', async () => {
  const res = await switchTo({ org: orgId, back: '/services/abc' });
  assert.equal(res.status, 303);
  assert.equal(res.headers.get('location'), '/services/abc');
  assert.ok(getSetCookies(res).some((c) => c.startsWith(`pilots_org=${encodeURIComponent(orgId)}`)));
});

test('a backslash after the slash is an open redirect and falls back', async () => {
  const res = await switchTo({ org: orgId, back: '/\\evil.com' });
  assert.equal(res.status, 303);
  assert.equal(res.headers.get('location'), '/machines', 'Chrome and Safari would resolve /\\evil.com as //evil.com');
});

test('a protocol-relative or absolute target falls back', async () => {
  for (const back of ['//evil.com', '//evil.com/x', 'https://evil.com', 'evil.com', '', '\\\\evil.com', '/']) {
    const res = await switchTo({ org: orgId, back });
    assert.equal(res.headers.get('location'), '/machines', `back=${JSON.stringify(back)} never leaves the origin`);
  }
});

test('an org the visitor is not a member of is refused without a redirect', async () => {
  const res = await switchTo({ org: 'not-mine', back: '/machines' });
  assert.equal(res.status, 403, 'the envelope status re-renders the page rather than redirecting');
  assert.equal(res.headers.get('location'), null);
  assert.equal(getSetCookies(res).some((c) => c.startsWith('pilots_org=')), false, 'and no cookie is written');
});
