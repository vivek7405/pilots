/**
 * The signed-in header's identity control.
 *
 * The bug this exists for: a personal org's slug IS the login. It is created
 * that way at sign-up, so the header rendered `vivek7405 vivek7405` for every
 * user who had never joined a second org, which is every new user.
 *
 * Counterfactual: change the layout's `me.org.personal` branch back to an
 * `orgs.length > 1` branch and the first test here goes red, because on a
 * personal org the slug and the login are the same string and both render.
 */

import assert from 'node:assert/strict';
import { after, before, test } from 'node:test';
import { asUser, bootApp, signInAs } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';

let app: TestApp;

before(async () => {
  app = await bootApp();
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

/** The bytes between <header> and </header> on a served page. */
async function header(cookie: string, path = '/machines'): Promise<string> {
  const res = await app.handle(new Request(`http://localhost${path}`, asUser(cookie)));
  assert.equal(res.status, 200, `${path} is served`);
  const body = await res.text();
  const start = body.indexOf('<header');
  const end = body.indexOf('</header>');
  assert.ok(start >= 0 && end > start, 'the page rendered a header');
  return body.slice(start, end);
}

function occurrences(haystack: string, needle: string): number {
  return haystack.split(needle).length - 1;
}

/**
 * The identity trigger's own markup: the button a signed-in visitor actually
 * reads, without the menu panel behind it.
 */
function trigger(chrome: string): string {
  const start = chrome.indexOf('<ui-dropdown-menu-trigger');
  const end = chrome.indexOf('</ui-dropdown-menu-trigger>');
  assert.ok(start >= 0 && end > start, 'the header rendered an identity trigger');
  return chrome.slice(start, end);
}

test('a user on a personal org sees their login exactly once', async () => {
  const cookie = await signInAs(app.handle, { id: 8100, login: 'solo-pilot' });
  const chrome = await header(cookie);
  const button = trigger(chrome);

  // Twice in the button's bytes: as the accessible name and as the visible
  // text. Never a third time, which is what the slug branch used to add,
  // because a personal org's slug IS the login.
  assert.equal(occurrences(button, 'solo-pilot'), 2, `the trigger reads: ${button}`);
  assert.match(button, /aria-label="Account: solo-pilot"/);
  assert.ok(!chrome.includes('solo-pilot · solo-pilot'), 'the slug is not repeated beside the login');
});

test('a user on a shared org sees the org named beside the login', async () => {
  const cookie = await signInAs(app.handle, { id: 8101, login: 'duo-pilot' });
  const { db } = await import('#db/connection.server.ts');
  const schema = await import('#db/schema.server.ts');
  const user = (await db.query.users.findMany()).find((u) => u.login === 'duo-pilot')!;
  const [shared] = await db
    .insert(schema.orgs)
    .values({ slug: 'acme', name: 'Acme', personal: false, ownerId: user.id })
    .returning();
  await db.insert(schema.memberships).values({ orgId: shared!.id, userId: user.id, role: 'owner' });

  // Act as the shared org.
  const switched = `${cookie}; pilots_org=${shared!.id}`;
  const button = trigger(await header(switched));

  // Which org a request acts as decides which fleet it touches, so a shared
  // org names itself on the button rather than only inside the menu.
  assert.equal(occurrences(button, 'duo-pilot'), 2, `the trigger reads: ${button}`);
  assert.ok(button.includes('acme'), 'a non-personal org names itself on the trigger');
});

test('a user in more than one org gets a radio group in the menu', async () => {
  const cookie = await signInAs(app.handle, { id: 8102, login: 'multi-pilot' });
  const { db } = await import('#db/connection.server.ts');
  const schema = await import('#db/schema.server.ts');
  const user = (await db.query.users.findMany()).find((u) => u.login === 'multi-pilot')!;
  const [second] = await db
    .insert(schema.orgs)
    .values({ slug: 'globex', name: 'Globex', personal: false, ownerId: user.id })
    .returning();
  await db.insert(schema.memberships).values({ orgId: second!.id, userId: user.id, role: 'member' });

  const chrome = await header(cookie);

  assert.match(chrome, /<ui-dropdown-menu-group aria-label="Organisation"/);
  assert.match(chrome, /type="radio"/);
  assert.ok(chrome.includes('globex'), 'the other org is offered');
  // The radio items cannot post; the form <org-switcher> submits must be there.
  assert.match(chrome, /data-org-switch/);
});

test('the header is fixed and its height is reserved', async () => {
  const cookie = await signInAs(app.handle, { id: 8103, login: 'layout-pilot' });
  const res = await app.handle(new Request('http://localhost/machines', asUser(cookie)));
  const body = await res.text();

  // Fixed, never sticky: sticky flickers on iOS WebKit during a soft
  // navigation, and a fixed header leaves flow, so the body must reserve it.
  assert.match(body, /<header\s+class="fixed /);
  assert.ok(!body.includes('sticky top-0'), 'no sticky header');
  assert.match(body, /--header-h: 56px;/);
  assert.match(body, /padding-top: var\(--header-h\);/);
  // The kit's dialog scroll lock publishes this; a fixed header has to opt in
  // or it slides sideways whenever a modal opens.
  assert.match(body, /var\(--wj-scrollbar-compensation, 0px\)/);
});

test('the account actions also exist as plain forms on /org', async () => {
  const cookie = await signInAs(app.handle, { id: 8104, login: 'noscript-pilot' });
  const res = await app.handle(new Request('http://localhost/org', asUser(cookie)));
  const body = await res.text();

  // The identity menu's panel is a popover, invisible with scripting off, so
  // the same sign out has to be reachable on a real page.
  assert.match(body, />Account</);
  assert.match(body, /action="\/api\/auth\/signout"/);
  assert.match(body, /<noscript><a href="\/org"/, 'the header points a scriptless visitor here');
});
