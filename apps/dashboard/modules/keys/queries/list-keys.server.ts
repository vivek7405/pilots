'use server';
/**
 * The org's keys as the page renders them.
 *
 * Neither the plaintext (never stored) nor the hash leaves this function. The
 * hash is a revoke handle, so a page has no use for it and a rendered page is
 * the easiest place for one to end up in a screenshot.
 */
import { desc, eq } from 'drizzle-orm';
import { db } from '#db/connection.server.ts';
import { apiKeys } from '#db/schema.server.ts';

export interface KeyRow {
  id: string;
  name: string;
  prefix: string;
  scopes: string[];
  createdAt: Date;
  lastUsedAt: Date | null;
  revokedAt: Date | null;
}

export async function listKeys(orgId: string): Promise<KeyRow[]> {
  const rows = await db
    .select()
    .from(apiKeys)
    .where(eq(apiKeys.orgId, orgId))
    .orderBy(desc(apiKeys.createdAt))
    .all();
  return rows.map((k) => ({
    id: k.id,
    name: k.name,
    prefix: k.prefix,
    scopes: k.scopes,
    createdAt: k.createdAt,
    lastUsedAt: k.lastUsedAt,
    revokedAt: k.revokedAt,
  }));
}
