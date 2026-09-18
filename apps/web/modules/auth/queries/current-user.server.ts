'use server';

/**
 * The signed-in user plus the org they are acting as, for a page to await.
 *
 * A `'use server'` query rather than a direct `requireOrg()` call from a page,
 * so the same read has one shape everywhere and a component could reach it over
 * RPC without a hand-written fetch. It returns null when signed out; the
 * segment middleware is what turns that into a redirect.
 */
import { requireOrg } from '../session.server.ts';
import type { OrgSummary, Role } from '../types.ts';

export interface CurrentUser {
  id: number;
  login: string;
  name: string | null;
  avatarUrl: string | null;
  org: OrgSummary;
  role: Role;
}

export async function currentUser(): Promise<CurrentUser | null> {
  const ctx = await requireOrg();
  if (!ctx) return null;
  return {
    id: ctx.user.id,
    login: ctx.user.login,
    name: ctx.user.name,
    avatarUrl: ctx.user.avatarUrl,
    org: { id: ctx.org.id, slug: ctx.org.slug, name: ctx.org.name, personal: ctx.org.personal },
    role: ctx.role,
  };
}
