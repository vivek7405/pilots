'use server';
/**
 * Give an address to a service that has none.
 *
 * Only reachable for a service created private or one that predates minted
 * addresses; every service created since gets one at create. Accepted exactly
 * once, which the fleet enforces: a service that already has an address
 * answers 409, because a URL that can change is not a URL anyone can rely on.
 */
import { requireOrg } from '#modules/auth/session.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { assertOwned } from '#modules/fleet/org-filter.server.ts';
import { backTo } from '#modules/services/utils/back.ts';

/** Lowercase alphanumerics and hyphens, starting and ending with one. */
const LABEL = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/;

export async function setAddress(formData: FormData) {
  const ctx = await requireOrg();
  if (!ctx) return { success: false, error: 'Sign in to continue.', status: 401 };

  const id = String(formData.get('service') || '').trim();
  const domain = String(formData.get('domain') || '').trim().toLowerCase();
  if (!LABEL.test(domain) || domain.length > 63) {
    return {
      success: false,
      fieldErrors: { domain: 'Lowercase letters, numbers and hyphens, up to 63 characters' },
    };
  }

  try {
    const service = await fleet.services.get(id);
    if (!assertOwned(ctx.org.id, service)) {
      return { success: false, error: 'No such service.', status: 404 };
    }
    await fleet.services.patch(id, { domain });
  } catch (err) {
    // The 409 for a service that already has one, and the 409 for a label
    // another service or a machine holds, both arrive here. The fleet's
    // message says which, so it is passed through rather than replaced.
    return { success: false, fieldErrors: { domain: (err as Error).message } };
  }
  return { success: true, redirect: backTo(formData, `/services/${id}`, 'saved') };
}
