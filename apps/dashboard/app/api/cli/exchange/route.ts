/**
 * `pilot login`: a GitHub access token in, a deploy key out.
 *
 * The token is verified with GitHub's CHECK-A-TOKEN endpoint,
 * `POST /applications/{client_id}/token` under HTTP basic auth with the App's
 * own client id and secret, rather than with `GET /user`.
 *
 * The difference is the whole point. `GET /user` accepts a token from ANY OAuth
 * app on GitHub, so a token leaked from an unrelated application would mint
 * pilots keys for its owner. The check endpoint answers 404 unless the token was
 * issued to THIS App, and its response carries the same `user` object, so the
 * stricter call costs nothing this app does not already hold.
 *
 * This is the one route `pilot login` depends on, and only for MINTING. A key
 * already on disk keeps working with this app down, because every host verifies
 * from its own replica.
 */

import { db } from '#db/connection.server.ts';
import { apiKeys, orgs } from '#db/schema.server.ts';
import { and, eq } from 'drizzle-orm';
import { upsertGithubUser } from '#modules/auth/session.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { fleetErrorResponse } from '#modules/fleet/org-filter.server.ts';
import { prefixOf } from '#modules/keys/scopes.ts';
import { invalidResponse, jsonBody, readJson, str } from '#modules/http/guards.server.ts';

/** The scopes a CLI login gets. `deploy` contains `machines`. */
const CLI_SCOPES = ['deploy'];

interface CheckTokenResponse {
  user?: {
    id?: number | string;
    login?: string;
    name?: string | null;
    email?: string | null;
    avatar_url?: string | null;
  };
}

export async function POST(req: Request): Promise<Response> {
  const token = str(await readJson(req), 'github_access_token');
  if (!token || token.length > 255) {
    return invalidResponse({ github_access_token: 'A GitHub access token is required' });
  }

  const clientId = process.env.AUTH_GITHUB_ID;
  const clientSecret = process.env.AUTH_GITHUB_SECRET;
  if (!clientId || !clientSecret) {
    return jsonBody({ error: 'this dashboard has no GitHub App configured' }, 503);
  }

  let checked: Response;
  try {
    checked = await fetch(`https://api.github.com/applications/${encodeURIComponent(clientId)}/token`, {
      method: 'POST',
      headers: {
        authorization: `Basic ${Buffer.from(`${clientId}:${clientSecret}`).toString('base64')}`,
        accept: 'application/vnd.github+json',
        'content-type': 'application/json',
        'x-github-api-version': '2022-11-28',
      },
      body: JSON.stringify({ access_token: token }),
    });
  } catch {
    return jsonBody({ error: 'could not reach GitHub' }, 502);
  }

  // 404 is GitHub's answer for "that token was not issued to this App". It is
  // the whole reason this endpoint is used instead of GET /user.
  if (checked.status === 404) {
    return jsonBody({ error: 'token not issued to this app' }, 401);
  }
  if (!checked.ok) {
    return jsonBody({ error: 'github rejected the token check', upstream_status: checked.status }, 502);
  }

  const body = (await checked.json()) as CheckTokenResponse;
  const ghUser = body.user;
  if (!ghUser?.id || !ghUser.login) {
    return jsonBody({ error: 'github returned no user for this token' }, 502);
  }

  const user = await upsertGithubUser({
    id: String(ghUser.id),
    login: ghUser.login,
    name: ghUser.name ?? null,
    email: ghUser.email ?? null,
    image: ghUser.avatar_url ?? null,
  });

  // The PERSONAL org, not merely one they own: a CLI login must land in the
  // same org every time, and an owner can belong to several.
  const org = await db
    .select()
    .from(orgs)
    .where(and(eq(orgs.ownerId, user.id), eq(orgs.personal, true)))
    .get();
  if (!org) return jsonBody({ error: 'no personal org for this account' }, 500);

  let created: Awaited<ReturnType<typeof fleet.apiKeys.create>>;
  try {
    created = await fleet.apiKeys.create({ org_id: org.id, scopes: CLI_SCOPES });
  } catch (err) {
    return fleetErrorResponse(err);
  }
  if (!created.key) return jsonBody({ error: 'the fleet returned no plaintext key' }, 502);

  await db.insert(apiKeys).values({
    orgId: org.id,
    name: `cli ${ghUser.login} ${new Date().toISOString().slice(0, 10)}`,
    prefix: prefixOf(created.key),
    hash: created.hash,
    scopes: CLI_SCOPES,
    // Null: a CLI login has no session, so there is no dashboard user to
    // attribute the key to beyond the org it belongs to.
    createdBy: null,
  });

  // The plaintext appears in this response and in no other.
  return jsonBody({ api_key: created.key, org_id: org.id, scopes: CLI_SCOPES });
}
