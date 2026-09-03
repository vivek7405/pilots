'use server';
/** Every org this user belongs to, for the switcher in the nav. */
import { eq } from 'drizzle-orm';
import { db } from '#db/connection.server.ts';
import { memberships, orgs } from '#db/schema.server.ts';
import type { OrgSummary } from '#modules/auth/types.ts';

export async function listOrgs(userId: number): Promise<OrgSummary[]> {
  const rows = await db
    .select()
    .from(memberships)
    .innerJoin(orgs, eq(memberships.orgId, orgs.id))
    .where(eq(memberships.userId, userId))
    .all();
  return rows.map((r) => ({
    id: r.orgs.id,
    slug: r.orgs.slug,
    name: r.orgs.name,
    personal: r.orgs.personal,
  }));
}
