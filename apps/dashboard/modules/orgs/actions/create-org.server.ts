'use server';
/**
 * Create a second team.
 *
 * Every account already has one team from its first sign-in, the personal one.
 * This is the shared one: a name, an address derived from it, and the creator
 * as its owner.
 *
 * The whole thing is ONE transaction, for the reason the sign-in path gives:
 * a team row with no membership belongs to nobody, cannot be switched into,
 * and cannot be deleted by anyone, so it would sit in the table forever. The
 * slug is picked inside that transaction too, so two creates racing on the
 * same name cannot both see the address as free.
 *
 * It returns a `Response` rather than an `ActionResult` on success, because
 * creating a team switches to it and the switch is a cookie, which has to ride
 * a header. `switchOrg` returns one for exactly the same reason. Every FAILURE
 * is still a returned envelope: a throw inside an action is sanitized to a
 * generic 500 in production and the caller loses the reason.
 */
import { db } from '#db/connection.server.ts';
import { memberships, orgs } from '#db/schema.server.ts';
import { freeSlug, orgCookie, requireUser } from '#modules/auth/session.server.ts';
import { slugify } from '#modules/orgs/slug.ts';

/** The longest a team name may be. Past this it is not a name, it is a paste. */
const NAME_MAX = 60;

export interface CreateOrgFailure {
  success: false;
  error?: string;
  fieldErrors?: Record<string, string>;
  status?: number;
}

export async function createOrg(formData: FormData): Promise<Response | CreateOrgFailure> {
  const user = await requireUser();
  if (!user) return { success: false, error: 'Sign in to continue.', status: 401 };

  const name = String(formData.get('name') || '').trim().slice(0, NAME_MAX);
  if (!name) return { success: false, fieldErrors: { name: 'Give the team a name' } };
  // A name of nothing but punctuation has no address, and inventing one would
  // hand back a URL the person never typed and cannot predict.
  if (!slugify(name)) {
    return { success: false, fieldErrors: { name: 'Use at least one letter or number' } };
  }

  const org = await db.transaction((tx) => {
    const [row] = tx
      .insert(orgs)
      .values({ slug: freeSlug(tx, name), name, personal: false, ownerId: user.id })
      .returning()
      .all();
    tx.insert(memberships).values({ userId: user.id, orgId: row.id, role: 'owner' }).run();
    return row;
  });

  // Act as the new team immediately. Creating one and landing back on the old
  // one reads as a create that did not happen.
  return new Response(null, {
    status: 303,
    headers: { location: '/org?ok=created', 'set-cookie': orgCookie(org.id) },
  });
}
