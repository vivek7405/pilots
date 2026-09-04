'use server';
/**
 * Every org the signed-in user belongs to, for the switcher in the nav.
 *
 * The user comes from the session, never from an argument: this is an RPC
 * endpoint, and a caller-supplied id would let anyone walk the integer user
 * ids and harvest every org on the fleet.
 */
import { eq } from 'drizzle-orm';
import { db } from '#db/connection.server.ts';
import { memberships, orgs } from '#db/schema.server.ts';
import { requireUser, signedOut } from '#modules/auth/session.server.ts';
import type { SignedOut } from '#modules/auth/session.server.ts';
import type { OrgSummary } from '#modules/auth/types.ts';

export async function listOrgs(): Promise<OrgSummary[] | SignedOut> {
  const user = await requireUser();
  if (!user) return signedOut();
  const rows = await db
    .select()
    .from(memberships)
    .innerJoin(orgs, eq(memberships.orgId, orgs.id))
    .where(eq(memberships.userId, user.id))
    .all();
  return rows.map((r) => ({
    id: r.orgs.id,
    slug: r.orgs.slug,
    name: r.orgs.name,
    personal: r.orgs.personal,
  }));
}
