'use server';
/**
 * Throw away a builder and the layer cache behind it.
 *
 * What the fleet does with this is destroy the team's builder on that host and
 * bump the team's build-cache epoch, so the NEXT build on EVERY host re-pulls
 * the cache from scratch rather than trusting what it has. That is the whole
 * reason the button exists: a poisoned or half-written cache makes builds fail
 * in a way no log on this side explains, and the only fix is to stop trusting
 * it fleet-wide.
 *
 * Nothing a person made is lost. A builder holds no source and no data; it is
 * rebuilt on the next deploy. What IS lost is the cache, so the next build is
 * slow -- which is why this is an owner-or-admin action and says so.
 *
 * Owner or admin, not every member: a member who resets a builder makes the
 * next build slow for everyone on the team.
 */
import { requireOrg } from '#modules/auth/session.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { canManageMembers } from '#modules/orgs/roles.ts';
import type { BuilderReset } from '#modules/builders/types.ts';

export async function resetBuilder(formData: FormData) {
  const ctx = await requireOrg();
  if (!ctx) return { success: false, error: 'Sign in to continue.', status: 401 };
  if (!canManageMembers(ctx.role)) {
    return { success: false, error: 'Only an owner or an admin can reset a builder.', status: 403 };
  }

  // The host id comes off the form and lands in a URL PATH, so it is checked
  // against the shape a host id has before it is interpolated -- not because
  // the route is unscoped (it resets this team's builder on that host and
  // nothing else, and `?org=` says which team) but because a path segment
  // built from an unchecked form field is how a request ends up somewhere
  // nobody wrote down.
  const host = String(formData.get('host') || '').trim();
  if (!/^[a-z0-9][a-z0-9-]{0,62}$/i.test(host)) {
    return { success: false, error: 'No such builder.', status: 404 };
  }

  try {
    await fleet.http.json<BuilderReset>(
      'POST',
      `/v1/builders/${encodeURIComponent(host)}/reset`,
      { query: { org: ctx.org.id } },
    );
  } catch (err) {
    return { success: false, error: `The fleet refused: ${(err as Error).message}`, status: 502 };
  }

  return { success: true, redirect: '/org?ok=builder-reset' };
}
