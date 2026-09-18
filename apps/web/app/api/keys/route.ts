/**
 * API keys: list, mint, revoke.
 *
 * The dashboard MINTS keys and never verifies one. hostd returns the plaintext
 * exactly once and keeps the sha256; every host then authenticates a request
 * from its own local replica of that hash. So this app is in no request path,
 * and killing its host changes nothing about whether the API answers.
 *
 * What this table holds is metadata: a prefix a human can recognise, the hash
 * so a revoke can name the key, the scopes and the timestamps. It never holds a
 * plaintext, and the plaintext leaves this process in exactly one response --
 * the one that minted it.
 */

import { and, desc, eq } from 'drizzle-orm';
import { rateLimit } from '@webjsdev/server';
import { db } from '#db/connection.server.ts';
import { apiKeys } from '#db/schema.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { fleetErrorResponse } from '#modules/fleet/org-filter.server.ts';
import { prefixOf, validateScopes } from '#modules/keys/scopes.ts';
import {
  forbiddenResponse,
  invalidResponse,
  isResponse,
  jsonBody,
  notFoundResponse,
  orgOr401,
  readJson,
  str,
} from '#modules/http/guards.server.ts';

const limited = rateLimit({ window: '1m', max: 10, trustProxy: true, key: 'keys:' });

export async function GET(req: Request): Promise<Response> {
  const ctx = await orgOr401(req);
  if (isResponse(ctx)) return ctx;

  const rows = await db
    .select()
    .from(apiKeys)
    .where(eq(apiKeys.orgId, ctx.org.id))
    .orderBy(desc(apiKeys.createdAt))
    .all();

  // Neither the plaintext (never stored) nor the hash (a revoke handle, and a
  // value nothing outside this app should hold) leaves in a list.
  return jsonBody({
    keys: rows.map((k) => ({
      id: k.id,
      name: k.name,
      prefix: k.prefix,
      scopes: k.scopes,
      created_at: k.createdAt,
      last_used_at: k.lastUsedAt,
      revoked_at: k.revokedAt,
    })),
  });
}

export async function POST(req: Request): Promise<Response> {
  return limited(req, async () => {
    const ctx = await orgOr401(req);
    if (isResponse(ctx)) return ctx;

    const raw = await readJson(req);
    const name = str(raw, 'name').slice(0, 100);
    if (!name) return invalidResponse({ name: 'Give the key a name' });

    const checked = validateScopes(raw.scopes, ctx.role);
    if ('error' in checked) {
      // A member asking for `admin` is a permission answer, not a shape one.
      return checked.error.startsWith('Only an org owner')
        ? forbiddenResponse(checked.error)
        : invalidResponse({ scopes: checked.error });
    }

    let created: Awaited<ReturnType<typeof fleet.apiKeys.create>>;
    try {
      created = await fleet.apiKeys.create({ org_id: ctx.org.id, scopes: checked.scopes });
    } catch (err) {
      return fleetErrorResponse(err);
    }
    if (!created.key) {
      return jsonBody({ error: 'the fleet returned no plaintext key' }, 502);
    }

    await db.insert(apiKeys).values({
      orgId: ctx.org.id,
      name,
      prefix: prefixOf(created.key),
      hash: created.hash,
      scopes: checked.scopes,
      createdBy: ctx.user.id,
    });

    // The one response that carries a plaintext. There is no second read of it
    // anywhere: the row holds only the hash.
    return jsonBody({
      key: created.key,
      prefix: prefixOf(created.key),
      scopes: checked.scopes,
      name,
    });
  });
}

export async function DELETE(req: Request): Promise<Response> {
  return limited(req, async () => {
    const ctx = await orgOr401(req);
    if (isResponse(ctx)) return ctx;

    const id = str(await readJson(req), 'id');
    if (!id) return invalidResponse({ id: 'A key id is required' });

    const row = await db
      .select()
      .from(apiKeys)
      .where(and(eq(apiKeys.id, id), eq(apiKeys.orgId, ctx.org.id)))
      .get();
    if (!row) return notFoundResponse('key');

    try {
      await fleet.apiKeys.revoke(row.hash);
    } catch (err) {
      return fleetErrorResponse(err);
    }

    // A tombstone, never a delete. Erasing the row would erase the record that
    // the key ever existed, which is the one fact an incident needs.
    await db.update(apiKeys).set({ revokedAt: new Date() }).where(eq(apiKeys.id, id));
    return jsonBody({ id, revoked: true });
  });
}
