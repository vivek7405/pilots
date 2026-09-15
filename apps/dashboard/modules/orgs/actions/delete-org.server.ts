'use server';
/**
 * Delete a team.
 *
 * Owner only, never a personal team, and never while the team still owns
 * anything on the fleet.
 *
 * The fleet is ASKED rather than assumed. This database holds no machines, no
 * services and no volumes -- they live in the fleet and are read on every
 * request -- so "is this team empty" is a question only the fleet can answer,
 * and deleting the team here would not delete them. What it would do is remove
 * the last row that says who those resources belong to, leaving them running,
 * billable, and reachable by nobody. So the refusal names what is left and
 * sends the owner to remove it first.
 *
 * A fleet that cannot be reached is a refusal too, not a pass. "I could not
 * check" and "there is nothing there" are different answers and only one of
 * them makes this safe.
 *
 * Every refusal is a RETURNED envelope rather than a throw: a throw inside an
 * action is sanitized to a generic 500 in production, and the whole point of
 * this one is to say what is still there.
 */
import { eq, isNull, and } from 'drizzle-orm';
import { db } from '#db/connection.server.ts';
import { apiKeys, memberships, orgs } from '#db/schema.server.ts';
import { requireOrg } from '#modules/auth/session.server.ts';
import { fleet, listMachines, listServices, listVolumes } from '#modules/fleet/client.server.ts';
import { canAdministerOrg } from '#modules/orgs/roles.ts';
import { NOUN } from '#lib/vocabulary.ts';

export async function deleteOrg(formData: FormData) {
  const ctx = await requireOrg();
  if (!ctx) return { success: false, error: 'Sign in to continue.', status: 401 };
  if (!canAdministerOrg(ctx.role)) {
    return { success: false, error: 'Only an owner can delete a team.', status: 403 };
  }
  if (ctx.org.personal) {
    return { success: false, error: 'A personal team cannot be deleted.', status: 422 };
  }

  // Typing the name is the confirmation. A team is deleted once, and the
  // person doing it should have had to read which one they were on.
  const typed = String(formData.get('confirm') || '').trim();
  if (typed !== ctx.org.name) {
    return { success: false, fieldErrors: { confirm: `Type ${ctx.org.name} to confirm` } };
  }

  let counts: { services: number; machines: number; volumes: number };
  try {
    const [services, machines, volumes] = await Promise.all([
      listServices(ctx.org.id),
      listMachines(ctx.org.id),
      listVolumes(ctx.org.id),
    ]);
    counts = { services: services.length, machines: machines.length, volumes: volumes.length };
  } catch {
    return {
      success: false,
      error: 'Could not check what this team still owns. Try again in a moment.',
      status: 502,
    };
  }

  const left = [
    counts.services > 0 ? `${counts.services} ${counts.services === 1 ? NOUN.App : NOUN.Apps}` : '',
    counts.machines > 0 ? `${counts.machines} running` : '',
    counts.volumes > 0 ? `${counts.volumes} ${NOUN.Storage}` : '',
  ].filter(Boolean);
  if (left.length > 0) {
    return {
      success: false,
      error: `This team still has ${left.join(', ')}. Remove them first.`,
      status: 409,
    };
  }

  // Every token this team minted stops working at the same moment the team
  // does. A live token for a team nobody can see is a credential with no owner
  // and no page that lists it.
  const live = await db
    .select()
    .from(apiKeys)
    .where(and(eq(apiKeys.orgId, ctx.org.id), isNull(apiKeys.revokedAt)))
    .all();
  for (const key of live) {
    try {
      await fleet.apiKeys.revoke(key.hash);
    } catch {
      return {
        success: false,
        error: 'Could not revoke this team’s tokens, so it was not deleted. Try again in a moment.',
        status: 502,
      };
    }
    await db.update(apiKeys).set({ revokedAt: new Date() }).where(eq(apiKeys.id, key.id));
  }

  // The membership rows go; the key rows stay with their `revoked_at`, which is
  // the record an operator reading an incident actually needs, and the billing
  // account stays for the same reason.
  await db.delete(memberships).where(eq(memberships.orgId, ctx.org.id));
  await db.delete(orgs).where(eq(orgs.id, ctx.org.id));

  return { success: true, redirect: '/org?ok=deleted' };
}
