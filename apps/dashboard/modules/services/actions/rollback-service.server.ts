'use server';
/**
 * Roll back to the previous healthy release.
 *
 * No target is sent. A release that never passed its health gate is not a
 * rollback target, and the engine is the only thing that knows which one did.
 */
import { requireOrg } from '#modules/auth/session.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { assertOwned } from '#modules/fleet/org-filter.server.ts';

export async function rollbackService(formData: FormData) {
  const ctx = await requireOrg();
  if (!ctx) return { success: false, error: 'Sign in to continue.', status: 401 };

  const id = String(formData.get('service') || '').trim();
  try {
    if (!assertOwned(ctx.org.id, await fleet.services.get(id))) {
      return { success: false, error: 'No such service.', status: 404 };
    }
    await fleet.services.rollback(id);
  } catch (err) {
    return { success: false, error: `Rollback refused: ${(err as Error).message}`, status: 502 };
  }
  return { success: true, redirect: `/services/${id}` };
}
