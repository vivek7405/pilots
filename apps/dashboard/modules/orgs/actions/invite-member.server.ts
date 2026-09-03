'use server';
/**
 * Add someone to the org by GitHub login.
 *
 * GitHub resolves the login to an id, so an invite cannot create a
 * placeholder account that a real person later has to be merged into: the row
 * this writes is keyed on the same `github_id` their first sign-in will carry.
 */
import { and, eq } from 'drizzle-orm';
import { db } from '#db/connection.server.ts';
import { memberships, users } from '#db/schema.server.ts';
import { requireOrg } from '#modules/auth/session.server.ts';

export async function inviteMember(formData: FormData) {
  const ctx = await requireOrg();
  if (!ctx) return { success: false, error: 'Sign in to continue.', status: 401 };
  if (ctx.role !== 'owner') return { success: false, error: 'Only an owner can invite.', status: 403 };

  const login = String(formData.get('login') || '').trim();
  if (!/^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$/.test(login)) {
    return { success: false, fieldErrors: { login: 'Enter a GitHub username' } };
  }

  let profile: { id?: number; login?: string; name?: string | null; avatar_url?: string | null };
  try {
    const res = await fetch(`https://api.github.com/users/${encodeURIComponent(login)}`, {
      headers: { accept: 'application/vnd.github+json', 'x-github-api-version': '2022-11-28' },
    });
    if (res.status === 404) return { success: false, fieldErrors: { login: 'No such GitHub user' } };
    if (!res.ok) return { success: false, error: 'GitHub could not be reached.', status: 502 };
    profile = (await res.json()) as typeof profile;
  } catch {
    return { success: false, error: 'GitHub could not be reached.', status: 502 };
  }
  if (!profile.id || !profile.login) return { success: false, error: 'GitHub returned no user.', status: 502 };

  const githubId = String(profile.id);
  const existing = await db.select().from(users).where(eq(users.githubId, githubId)).get();
  const [user] = existing
    ? [existing]
    : await db
        .insert(users)
        .values({
          githubId,
          login: profile.login,
          name: profile.name ?? null,
          avatarUrl: profile.avatar_url ?? null,
        })
        .returning();

  const already = await db
    .select()
    .from(memberships)
    .where(and(eq(memberships.userId, user.id), eq(memberships.orgId, ctx.org.id)))
    .get();
  if (already) return { success: false, fieldErrors: { login: 'Already a member' } };

  await db.insert(memberships).values({ userId: user.id, orgId: ctx.org.id, role: 'member' });
  return { success: true, redirect: '/org' };
}
