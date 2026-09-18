'use server';
/**
 * Mint a key from the keys page.
 *
 * The plaintext comes back on `data` so the page can show it ONCE, in the
 * banner that renders from `actionData`. It is gone on the next render because
 * nothing stored it: the row holds the hash.
 */
import { db } from '#db/connection.server.ts';
import { apiKeys } from '#db/schema.server.ts';
import { requireOrg } from '#modules/auth/session.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { prefixOf, validateScopes } from '#modules/keys/scopes.ts';

export interface CreateKeyResult {
  success: boolean;
  data?: { key: string; prefix: string; name: string };
  error?: string;
  fieldErrors?: Record<string, string>;
  status?: number;
}

export async function createKey(formData: FormData): Promise<CreateKeyResult> {
  const ctx = await requireOrg();
  if (!ctx) return { success: false, error: 'Sign in to continue.', status: 401 };

  const name = String(formData.get('name') || '').trim().slice(0, 100);
  const scopes = formData.getAll('scopes').map(String);
  if (!name) return { success: false, fieldErrors: { name: 'Give the key a name' } };

  const checked = validateScopes(scopes, ctx.role);
  if ('error' in checked) {
    return checked.error.startsWith('Only an org owner')
      ? { success: false, error: checked.error, status: 403 }
      : { success: false, fieldErrors: { scopes: checked.error } };
  }

  let created;
  try {
    created = await fleet.apiKeys.create({ org_id: ctx.org.id, scopes: checked.scopes });
  } catch (err) {
    return { success: false, error: `The fleet refused: ${(err as Error).message}`, status: 502 };
  }
  if (!created.key) return { success: false, error: 'The fleet returned no key.', status: 502 };

  await db.insert(apiKeys).values({
    orgId: ctx.org.id,
    name,
    prefix: prefixOf(created.key),
    hash: created.hash,
    scopes: checked.scopes,
    createdBy: ctx.user.id,
  });

  // No `redirect`: a PRG would discard the plaintext, which the page has to
  // render once and can never fetch again.
  return { success: true, data: { key: created.key, prefix: prefixOf(created.key), name } };
}
