'use server';
/** Who is in this org, and at what role. */
import { eq } from 'drizzle-orm';
import { db } from '#db/connection.server.ts';
import { memberships, users } from '#db/schema.server.ts';
import type { Role } from '#modules/auth/types.ts';

export interface MemberRow {
  userId: number;
  login: string;
  name: string | null;
  avatarUrl: string | null;
  role: Role;
  since: Date;
}

export async function listMembers(orgId: string): Promise<MemberRow[]> {
  const rows = await db
    .select()
    .from(memberships)
    .innerJoin(users, eq(memberships.userId, users.id))
    .where(eq(memberships.orgId, orgId))
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
