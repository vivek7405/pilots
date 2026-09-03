/**
 * Who is signed in, and which org they are acting as.
 *
 * A server-only utility (no `'use server'`): pages, route handlers, `WS`
 * handlers and actions all call it server-to-server, and nothing here is
 * reachable from the browser.
 *
 * The org selection rides an UNSIGNED `pilots_org` cookie, which is safe
 * because it is never trusted on its own: `currentOrg` re-reads the membership
 * table on every request, so a forged value can only ever select an org the
 * visitor already belongs to. Signing it would buy nothing and add a second
 * secret to rotate.
 */

import { and, eq } from 'drizzle-orm';
import { db } from '#db/connection.server.ts';
import { memberships, orgs, users } from '#db/schema.server.ts';
import type { Membership, Org, User } from '#db/schema.server.ts';
import { auth } from './auth.server.ts';
import type { Role } from './types.ts';

export const ORG_COOKIE = 'pilots_org';

/** The identity the OAuth provider's `profile` mapping produces. */
export interface GithubIdentity {
  id: string;
  login: string;
  name?: string | null;
  email?: string | null;
  image?: string | null;
}

/** A resolved request context: the signed-in user and the org they act as. */
export interface OrgContext {
  user: User;
  org: Org;
  role: Role;
}

/**
 * Insert or refresh the `users` row for a GitHub identity, and on the FIRST
 * sight also create that user's personal org and their `owner` membership.
 *
 * One transaction, because a user row with no org would leave the account
 * signed in and unable to see anything, with no path to repair itself.
 */
export async function upsertGithubUser(identity: GithubIdentity): Promise<User> {
  const githubId = String(identity.id);
  const now = new Date();
  return db.transaction((tx) => {
    const [row] = tx
      .insert(users)
      .values({
        githubId,
        login: identity.login,
        name: identity.name ?? null,
        email: identity.email ?? null,
        avatarUrl: identity.image ?? null,
      })
      .onConflictDoUpdate({
        target: users.githubId,
        set: {
          login: identity.login,
          name: identity.name ?? null,
          email: identity.email ?? null,
          avatarUrl: identity.image ?? null,
          updatedAt: now,
        },
      })
      .returning()
      .all();

    const existing = tx.select().from(orgs).where(eq(orgs.ownerId, row.id)).all();
    if (existing.some((o) => o.personal)) return row;

    // The slug is the GitHub login, which GitHub already guarantees unique.
    const [org] = tx
      .insert(orgs)
      .values({ slug: identity.login, name: identity.login, personal: true, ownerId: row.id })
      .returning()
      .all();
    tx.insert(memberships).values({ userId: row.id, orgId: org.id, role: 'owner' }).run();
    return row;
  });
}

/** The `users` row for the session on this request, or null when signed out. */
export async function requireUser(req?: Request): Promise<User | null> {
  const session = await auth(req);
  const githubId = session?.user?.id;
  if (!githubId) return null;
  const row = await db.query.users.findFirst({ where: { githubId: String(githubId) } });
  return row ?? null;
}

/**
 * The org this request acts as: the `pilots_org` cookie when the user is a
 * member of it, else their personal org, else any org they belong to.
 */
export async function currentOrg(req: Request | undefined, user: User): Promise<{ org: Org; role: Role } | null> {
  const rows = await db
    .select()
    .from(memberships)
    .innerJoin(orgs, eq(memberships.orgId, orgs.id))
    .where(eq(memberships.userId, user.id))
    .all();
  if (rows.length === 0) return null;

  const asPairs: { org: Org; role: Role }[] = rows.map((r: { memberships: Membership; orgs: Org }) => ({
    org: r.orgs,
    role: r.memberships.role === 'owner' ? 'owner' : 'member',
  }));

  const wanted = readOrgCookie(req);
  return (
    asPairs.find((p) => p.org.id === wanted) ??
    asPairs.find((p) => p.org.personal) ??
    asPairs[0]
  );
}

/** Both halves, or null when the request is not authenticated. */
export async function requireOrg(req?: Request): Promise<OrgContext | null> {
  const user = await requireUser(req);
  if (!user) return null;
  const picked = await currentOrg(req, user);
  if (!picked) return null;
  return { user, org: picked.org, role: picked.role };
}

/** Whether a user holds a membership on an org, and at what role. */
export async function roleOn(userId: number, orgId: string): Promise<Role | null> {
  const row = await db
    .select()
    .from(memberships)
    .where(and(eq(memberships.userId, userId), eq(memberships.orgId, orgId)))
    .get();
  if (!row) return null;
  return row.role === 'owner' ? 'owner' : 'member';
}

/** The `Set-Cookie` value that switches the acting org. */
export function orgCookie(orgId: string): string {
  return `${ORG_COOKIE}=${encodeURIComponent(orgId)}; Path=/; HttpOnly; SameSite=Lax; Max-Age=31536000`;
}

function readOrgCookie(req?: Request): string | null {
  const header = req?.headers.get('cookie');
  if (!header) return null;
  for (const part of header.split(';')) {
    const [name, ...rest] = part.trim().split('=');
    if (name === ORG_COOKIE) return decodeURIComponent(rest.join('='));
  }
  return null;
}
