'use server';
/** Revoke a key: tell the fleet the hash, then tombstone the row. */
import { and, eq } from 'drizzle-orm';
import { db } from '#db/connection.server.ts';
import { apiKeys } from '#db/schema.server.ts';
import { requireOrg } from '#modules/auth/session.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';

export async function revokeKey(formData: FormData) {
  const ctx = await requireOrg();
  if (!ctx) return { success: false, error: 'Sign in to continue.', status: 401 };

  const id = String(formData.get('id') || '').trim();
  const row = await db
    .select()
    .from(apiKeys)
    .where(and(eq(apiKeys.id, id), eq(apiKeys.orgId, ctx.org.id)))
    .get();
  if (!row) return { success: false, error: 'No such key.', status: 404 };

  try {
    await fleet.apiKeys.revoke(row.hash);
  } catch (err) {
    return { success: false, error: `The fleet refused: ${(err as Error).message}`, status: 502 };
  }

  // The row stays. A delete would erase the record that the key existed.
  await db.update(apiKeys).set({ revokedAt: new Date() }).where(eq(apiKeys.id, id));
  return { success: true, redirect: '/keys' };
}
