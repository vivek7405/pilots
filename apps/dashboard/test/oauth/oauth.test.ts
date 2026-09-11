/**
 * The OAuth 2.1 authorization server, end to end through the real pipeline.
 *
 * The properties under test are the ones that decide whether this is a way to
 * hand an agent a scoped token or a way to steal one:
 *
 *   - PKCE is required and S256 only, so a public client proves itself.
 *   - A code is single-use, short-lived, and bound to the client, the redirect
 *     URI and the challenge.
 *   - A redirect URI is matched EXACTLY against what was registered, and a
 *     request that fails that check is never redirected anywhere.
 *   - The approve endpoint takes its parameters from the SIGNED ticket, not
 *     from the form, so a tampered field changes nothing, and a forged form
 *     has no ticket at all.
 *   - The token that comes out is an ordinary pilots key, carrying the
 *     restrictions the human chose.
 */

import assert from 'node:assert/strict';
import { createHash, randomBytes } from 'node:crypto';
import { after, before, test } from 'node:test';

import { bootApp, signInAs } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';

let app: TestApp;
let db: typeof import('#db/connection.server.ts')['db'];

const REDIRECT = 'http://127.0.0.1:53682/callback';

function base64url(buf: Buffer): string {
  return buf.toString('base64url');
}

function pkce(): { verifier: string; challenge: string } {
  const verifier = base64url(randomBytes(32));
  return { verifier, challenge: base64url(createHash('sha256').update(verifier).digest()) };
}

async function registerClient(redirectUris: string[] = [REDIRECT]): Promise<string> {
  const res = await app.handle(
    new Request('http://localhost/api/oauth/register', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ client_name: 'Test Agent', redirect_uris: redirectUris }),
    }),
  );
  // Read the body ONCE: a Response body is a stream, and asserting with
  // `await res.text()` in the message consumes it before the parse.
  const text = await res.text();
  assert.equal(res.status, 201, text);
  const body = JSON.parse(text) as { client_id: string; token_endpoint_auth_method: string };
  assert.equal(body.token_endpoint_auth_method, 'none', 'a public client must not be issued a secret');
  return body.client_id;
}

function authorizeUrl(clientId: string, challenge: string, extra: Record<string, string> = {}): string {
  const params = new URLSearchParams({
    response_type: 'code',
    client_id: clientId,
    redirect_uri: REDIRECT,
    code_challenge: challenge,
    code_challenge_method: 'S256',
    scope: 'machines',
    state: 'xyz',
    ...extra,
  });
  return `http://localhost/oauth/authorize?${params.toString()}`;
}

before(async () => {
  app = await bootApp();
  ({ db } = await import('#db/connection.server.ts'));
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

test('the metadata names this origin as the issuer and every endpoint on it', async () => {
  const res = await app.handle(new Request('http://localhost/api/oauth/metadata'));
  assert.equal(res.status, 200);
  assert.equal(res.headers.get('access-control-allow-origin'), '*', 'a browser client reads it cross-origin');

  const doc = (await res.json()) as Record<string, unknown>;
  assert.equal(doc.issuer, 'http://localhost');
  assert.equal(doc.authorization_endpoint, 'http://localhost/oauth/authorize');
  assert.equal(doc.token_endpoint, 'http://localhost/api/oauth/token');
  assert.equal(doc.registration_endpoint, 'http://localhost/api/oauth/register');
  // S256 only: `plain` proves nothing, so it is not advertised and not accepted.
  assert.deepEqual(doc.code_challenge_methods_supported, ['S256']);
  assert.deepEqual(doc.grant_types_supported, ['authorization_code']);
  assert.deepEqual(doc.token_endpoint_auth_methods_supported, ['none']);
});

test('the issuer follows the forwarded host, because it is an identity', async () => {
  const res = await app.handle(
    new Request('http://localhost/api/oauth/metadata', {
      headers: { 'x-forwarded-host': 'pilots.example', 'x-forwarded-proto': 'https' },
    }),
  );
  const doc = (await res.json()) as { issuer: string; token_endpoint: string };
  assert.equal(doc.issuer, 'https://pilots.example');
  assert.equal(doc.token_endpoint, 'https://pilots.example/api/oauth/token');
});

test('registration refuses a redirect URI that is not https, loopback or private-use', async () => {
  for (const uri of ['http://evil.example/callback', 'javascript:alert(1)', 'not a url']) {
    const res = await app.handle(
      new Request('http://localhost/api/oauth/register', {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ client_name: 'Bad', redirect_uris: [uri] }),
      }),
    );
    assert.equal(res.status, 400, `${uri} was accepted`);
    assert.match((await res.text()), /invalid_redirect_uri/);
  }
  // https, loopback http and a private-use scheme are all fine.
  await registerClient(['https://app.example/cb', 'http://127.0.0.1:1234/cb', 'com.example.app:/cb']);
});

test('an authorize request with an unregistered redirect is shown to the USER, never redirected', async () => {
  const clientId = await registerClient();
  const { challenge } = pkce();
  const url = authorizeUrl(clientId, challenge).replace(
    encodeURIComponent(REDIRECT),
    encodeURIComponent('http://127.0.0.1:53682/somewhere-else'),
  );
  const res = await app.handle(new Request(url));
  // 200 with an explanation, and NOT a 3xx: the only address available to
  // redirect to is the unvalidated one in the request.
  assert.equal(res.status, 200);
  const html = await res.text();
  assert.match(html, /not registered/i);
  assert.doesNotMatch(html, /somewhere-else/, 'the unvalidated URI must not become a link');
});

test('an unknown client is shown to the user, and a bad PKCE goes back to the client', async () => {
  const unknown = await app.handle(new Request(authorizeUrl('no-such-client', 'x')));
  assert.equal(unknown.status, 200);
  assert.match(await unknown.text(), /Unknown application/);

  const clientId = await registerClient();
  // `plain` is refused rather than downgraded to.
  const plain = await app.handle(
    new Request(authorizeUrl(clientId, 'whatever', { code_challenge_method: 'plain' })),
  );
  const html = await plain.text();
  assert.match(html, /S256/);
  // Once the client and the URI are verified, a refusal DOES go back to the
  // client, on its own registered address, with the state echoed.
  assert.match(html, new RegExp(REDIRECT.replace(/[/:]/g, '\\$&')));
  assert.match(html, /state=xyz/);
});

test('the whole flow: consent, code, token, and the token is a pilots key', async () => {
  const clientId = await registerClient();
  const { verifier, challenge } = pkce();

  // A signed-in user gets the consent screen with a ticket in it.
  const { cookie, userId, orgId } = await signIn();
  const page = await app.handle(new Request(authorizeUrl(clientId, challenge), { headers: { cookie } }));
  assert.equal(page.status, 200);
  const html = await page.text();
  const ticket = /name="ticket" value="([^"]+)"/.exec(html)?.[1];
  assert.ok(ticket, 'the consent screen carries no ticket');
  assert.match(html, /Authorize Test Agent/);

  // Approving posts the ticket and the three restrictions the human chose.
  const approve = await app.handle(
    new Request('http://localhost/api/oauth/approve', {
      method: 'POST',
      headers: { cookie, 'content-type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({
        ticket,
        decision: 'approve',
        org: orgId,
        prefix: 'mcp-',
        max_machines: '5',
        lifetime: '24',
      }),
    }),
  );
  assert.equal(approve.status, 303);
  const location = new URL(approve.headers.get('location')!);
  assert.equal(location.origin + location.pathname, REDIRECT);
  assert.equal(location.searchParams.get('state'), 'xyz');
  const code = location.searchParams.get('code');
  assert.ok(code, 'no code was issued');

  // The exchange. The key that comes back is minted on the fleet with the
  // restrictions attached.
  app.fleet.calls.length = 0;
  const token = await app.handle(
    new Request('http://localhost/api/oauth/token', {
      method: 'POST',
      headers: { 'content-type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({
        grant_type: 'authorization_code',
        code,
        client_id: clientId,
        redirect_uri: REDIRECT,
        code_verifier: verifier,
      }),
    }),
  );
  const tokenText = await token.text();
  assert.equal(token.status, 200, tokenText);
  const body = JSON.parse(tokenText) as {
    access_token: string;
    token_type: string;
    scope: string;
    expires_in?: number;
  };
  assert.match(body.access_token, /^pilot_/, 'the access token IS a pilots key');
  assert.equal(body.token_type, 'Bearer');
  assert.equal(body.scope, 'machines');
  assert.ok(body.expires_in && body.expires_in > 0, 'a chosen lifetime must reach the client');

  const minted = app.fleet.calls.find((c) => c.method === 'apiKeys.create');
  assert.ok(minted, 'no key was minted on the fleet');
  assert.equal((minted.args[0] as { name_prefix?: string }).name_prefix, 'mcp-');
  assert.equal((minted.args[0] as { max_machines?: number }).max_machines, 5);
  assert.ok((minted.args[0] as { expires_at?: number }).expires_at, 'the lifetime did not reach the fleet');

  // It shows up on the tokens page, attributed to the client that got it.
  const row = await db.query.apiKeys.findFirst({ where: { clientId } });
  assert.ok(row, 'the key was not recorded for the tokens page');
  assert.equal(row.createdBy, userId);
  assert.match(row.name, /oauth Test Agent/);

  // ONE code, ONE token. A replay is refused.
  const replay = await app.handle(
    new Request('http://localhost/api/oauth/token', {
      method: 'POST',
      headers: { 'content-type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({
        grant_type: 'authorization_code',
        code,
        client_id: clientId,
        redirect_uri: REDIRECT,
        code_verifier: verifier,
      }),
    }),
  );
  assert.equal(replay.status, 400);
  assert.match(await replay.text(), /already exchanged/);
});

test('the token endpoint refuses a wrong verifier, a wrong client and a wrong redirect', async () => {
  const clientId = await registerClient();
  const other = await registerClient();
  const { verifier, challenge } = pkce();
  const { cookie, orgId } = await signIn();

  const issue = async (): Promise<string> => {
    const page = await app.handle(new Request(authorizeUrl(clientId, challenge), { headers: { cookie } }));
    const ticket = /name="ticket" value="([^"]+)"/.exec(await page.text())?.[1]!;
    const approve = await app.handle(
      new Request('http://localhost/api/oauth/approve', {
        method: 'POST',
        headers: { cookie, 'content-type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({ ticket, decision: 'approve', org: orgId, lifetime: '0' }),
      }),
    );
    return new URL(approve.headers.get('location')!).searchParams.get('code')!;
  };

  const exchange = (fields: Record<string, string>) =>
    app.handle(
      new Request('http://localhost/api/oauth/token', {
        method: 'POST',
        headers: { 'content-type': 'application/x-www-form-urlencoded' },
        body: new URLSearchParams({
          grant_type: 'authorization_code',
          client_id: clientId,
          redirect_uri: REDIRECT,
          code_verifier: verifier,
          ...fields,
        }),
      }),
    );

  const wrongVerifier = await exchange({ code: await issue(), code_verifier: base64url(randomBytes(32)) });
  assert.equal(wrongVerifier.status, 400);
  assert.match(await wrongVerifier.text(), /code_verifier/);

  const wrongClient = await exchange({ code: await issue(), client_id: other });
  assert.equal(wrongClient.status, 400);
  assert.match(await wrongClient.text(), /another client/);

  const wrongRedirect = await exchange({ code: await issue(), redirect_uri: 'http://127.0.0.1:1/cb' });
  assert.equal(wrongRedirect.status, 400);

  // And a grant type this server does not implement is named, not ignored.
  const wrongGrant = await app.handle(
    new Request('http://localhost/api/oauth/token', {
      method: 'POST',
      headers: { 'content-type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({ grant_type: 'refresh_token', refresh_token: 'x' }),
    }),
  );
  assert.equal(wrongGrant.status, 400);
  assert.match(await wrongGrant.text(), /unsupported_grant_type/);
});

test('approve refuses a forged form, a tampered scope and a team the user is not in', async () => {
  const clientId = await registerClient();
  const { challenge } = pkce();
  const { cookie, orgId } = await signIn();

  // No ticket at all: the CSRF case, which is a POST from another page.
  const forged = await app.handle(
    new Request('http://localhost/api/oauth/approve', {
      method: 'POST',
      headers: { cookie, 'content-type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({ decision: 'approve', org: orgId }),
    }),
  );
  assert.equal(forged.status, 400);
  assert.match(await forged.text(), /no longer valid/);

  // A ticket whose payload was edited fails its signature.
  const page = await app.handle(new Request(authorizeUrl(clientId, challenge), { headers: { cookie } }));
  const ticket = /name="ticket" value="([^"]+)"/.exec(await page.text())?.[1]!;
  const [payload, mac] = ticket.split('.');
  const edited = JSON.parse(Buffer.from(payload, 'base64url').toString()) as Record<string, unknown>;
  edited.scopes = ['admin'];
  const tampered = `${Buffer.from(JSON.stringify(edited)).toString('base64url')}.${mac}`;
  const res = await app.handle(
    new Request('http://localhost/api/oauth/approve', {
      method: 'POST',
      headers: { cookie, 'content-type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({ ticket: tampered, decision: 'approve', org: orgId }),
    }),
  );
  assert.equal(res.status, 400);

  // A valid ticket, but a team the signed-in user does not belong to.
  const foreign = await app.handle(
    new Request('http://localhost/api/oauth/approve', {
      method: 'POST',
      headers: { cookie, 'content-type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({ ticket, decision: 'approve', org: 'someone-elses-org' }),
    }),
  );
  assert.equal(foreign.status, 403);

  // Signed out entirely.
  const anon = await app.handle(
    new Request('http://localhost/api/oauth/approve', {
      method: 'POST',
      headers: { 'content-type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({ ticket, decision: 'approve', org: orgId }),
    }),
  );
  assert.equal(anon.status, 401);
});

test('declining sends access_denied back to the client rather than a code', async () => {
  const clientId = await registerClient();
  const { challenge } = pkce();
  const { cookie, orgId } = await signIn();
  const page = await app.handle(new Request(authorizeUrl(clientId, challenge), { headers: { cookie } }));
  const ticket = /name="ticket" value="([^"]+)"/.exec(await page.text())?.[1]!;

  const res = await app.handle(
    new Request('http://localhost/api/oauth/approve', {
      method: 'POST',
      headers: { cookie, 'content-type': 'application/x-www-form-urlencoded' },
      body: new URLSearchParams({ ticket, decision: 'deny', org: orgId }),
    }),
  );
  assert.equal(res.status, 303);
  const location = new URL(res.headers.get('location')!);
  assert.equal(location.searchParams.get('error'), 'access_denied');
  assert.equal(location.searchParams.get('code'), null);
  assert.equal(location.searchParams.get('state'), 'xyz');
});

test('a signed-out visitor is offered sign-in and comes back to the same request', async () => {
  const clientId = await registerClient();
  const { challenge } = pkce();
  const res = await app.handle(new Request(authorizeUrl(clientId, challenge)));
  assert.equal(res.status, 200);
  const html = await res.text();
  assert.match(html, /Sign in to continue/);
  assert.match(html, /next=.*oauth%2Fauthorize/, 'the sign-in link does not carry the request back');
});

test('the OAuth endpoints a client reaches with no session are not behind the gate', async () => {
  // The gate is the api middleware; these four must pass it, and approve must
  // not.
  for (const path of ['/api/oauth/metadata']) {
    const res = await app.handle(new Request(`http://localhost${path}`));
    assert.notEqual(res.status, 401, `${path} is behind the session gate`);
  }
  const approve = await app.handle(new Request('http://localhost/api/oauth/approve', { method: 'POST' }));
  assert.equal(approve.status, 401, 'approve must stay behind the session gate');
});

/**
 * A signed-in visitor, through the app's own sign-in round trip: the same
 * path every other suite uses, so the session this returns is the one the
 * gate really reads.
 */
let nextUserId = 900;
async function signIn(): Promise<{ cookie: string; userId: number; orgId: string }> {
  const id = nextUserId++;
  const login = `oauth-user-${id}`;
  const cookie = await signInAs(app.handle, { id, login, name: 'OAuth User' });
  const user = await db.query.users.findFirst({ where: { githubId: String(id) } });
  assert.ok(user, 'the sign-in created no user');
  const org = await db.query.orgs.findFirst({ where: { ownerId: user.id, personal: true } });
  assert.ok(org, 'the sign-in created no personal team');
  return { cookie, userId: user.id, orgId: org.id };
}
