'use server';
/**
 * Rename a team.
 *
 * The address moves with the name. The switcher, the team page and every place
 * a person recognises their team by show the slug, so a rename that left the
 * old address behind would show the old name on the very screens the rename
 * was performed from.
 *
 * Nothing in this app or on the fleet looks a team up by slug -- the id is what
 * every row and every fleet request carries -- which is what makes moving it
 * safe. The one case that must not move it is a name whose address is already
 * the team's own: re-deriving there would find the slug taken (by this team)
 * and hand back a suffixed `acme-2`.
 *
 * Owner only. A rename is not a day-to-day act: it changes what the team is
 * called in every place its people already recognise it.
 */
import { eq } from 'drizzle-orm';
import { db } from '#db/connection.server.ts';
import { orgs } from '#db/schema.server.ts';
import { freeSlug, requireOrg } from '#modules/auth/session.server.ts';
import { canAdministerOrg } from '#modules/orgs/roles.ts';
import { slugify } from '#modules/orgs/slug.ts';

const NAME_MAX = 60;

export async function renameOrg(formData: FormData) {
  const ctx = await requireOrg();
  if (!ctx) return { success: false, error: 'Sign in to continue.', status: 401 };
  if (!canAdministerOrg(ctx.role)) {
    return { success: false, error: 'Only an owner can rename a team.', status: 403 };
  }

  const name = String(formData.get('name') || '').trim().slice(0, NAME_MAX);
  if (!name) return { success: false, fieldErrors: { name: 'Give the team a name' } };
  if (!slugify(name)) {
    return { success: false, fieldErrors: { name: 'Use at least one letter or number' } };
  }
  if (name === ctx.org.name) return { success: true, redirect: '/org?ok=saved' };

  const orgId = ctx.org.id;
  const currentSlug = ctx.org.slug;
  await db.transaction((tx) => {
    const slug = slugify(name) === currentSlug ? currentSlug : freeSlug(tx, name);
    tx.update(orgs).set({ name, slug }).where(eq(orgs.id, orgId)).run();
  });

  return { success: true, redirect: '/org?ok=saved' };
}
