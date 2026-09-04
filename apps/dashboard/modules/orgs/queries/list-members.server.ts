'use server';
/** Who is in the acting org, and at what role. The org comes from the session. */
import { eq } from 'drizzle-orm';
import { db } from '#db/connection.server.ts';
import { memberships, users } from '#db/schema.server.ts';
import { requireOrg, signedOut } from '#modules/auth/session.server.ts';
import type { SignedOut } from '#modules/auth/session.server.ts';
import type { Role } from '#modules/auth/types.ts';

export interface MemberRow {
  userId: number;
  login: string;
  name: string | null;
  avatarUrl: string | null;
  role: Role;
  since: Date;
}

export async function listMembers(): Promise<MemberRow[] | SignedOut> {
  const ctx = await requireOrg();
  if (!ctx) return signedOut();
  const rows = await db
    .select()
    .from(memberships)
    .innerJoin(users, eq(memberships.userId, users.id))
    .where(eq(memberships.orgId, ctx.org.id))
    .all();
  return rows.map((r) => ({
    userId: r.users.id,
    login: r.users.login,
    name: r.users.name,
    avatarUrl: r.users.avatarUrl,
    role: r.memberships.role === 'owner' ? 'owner' : 'member',
    since: r.memberships.createdAt,
  }));
}
