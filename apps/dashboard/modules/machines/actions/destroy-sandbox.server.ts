'use server';
/**
 * Remove a sandbox from the playground. Ownership is checked before the
 * destroy, and a machine the org does not own is a 404, never a 403.
 */
import { requireOrg } from '#modules/auth/session.server.ts';
import { fleet, fleetAs } from '#modules/fleet/client.server.ts';
import { assertOwned } from '#modules/fleet/org-filter.server.ts';

export async function destroySandbox(formData: FormData) {
  const ctx = await requireOrg();
  if (!ctx) return { success: false, error: 'Sign in to continue.', status: 401 };
  const id = String(formData.get('machine') || '').trim();
  try {
    if (!assertOwned(ctx.org.id, await fleet.machines.get(id))) {
      return { success: false, error: 'No such sandbox.', status: 404 };
    }
    await fleetAs(ctx.org.id).machines.destroy(id);
  } catch (err) {
    return { success: false, error: `Could not remove it: ${(err as Error).message}`, status: 502 };
  }
  return { success: true, redirect: '/sandboxes/playground?ok=destroyed' };
}
