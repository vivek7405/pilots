'use server';
/**
 * The acting org's keys as the page renders them. The org comes from the
 * session, never from an argument.
 *
 * Neither the plaintext (never stored) nor the hash leaves this function. The
 * hash is a revoke handle, so a page has no use for it and a rendered page is
 * the easiest place for one to end up in a screenshot.
 */
import { desc, eq } from 'drizzle-orm';
import { db } from '#db/connection.server.ts';
import { apiKeys } from '#db/schema.server.ts';
import { requireOrg, signedOut } from '#modules/auth/session.server.ts';
import type { SignedOut } from '#modules/auth/session.server.ts';

export interface KeyRow {
  id: string;
  name: string;
  prefix: string;
  scopes: string[];
  createdAt: Date;
  lastUsedAt: Date | null;
  revokedAt: Date | null;
}

export async function listKeys(): Promise<KeyRow[] | SignedOut> {
  const ctx = await requireOrg();
  if (!ctx) return signedOut();
  const rows = await db
    .select()
    .from(apiKeys)
    .where(eq(apiKeys.orgId, ctx.org.id))
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
