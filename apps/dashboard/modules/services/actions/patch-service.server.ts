'use server';
/** The two service knobs a page offers: replica count and autodeploy. */
import { requireOrg } from '#modules/auth/session.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { assertOwned } from '#modules/fleet/org-filter.server.ts';
import type { UpdateServiceRequest } from '@pilots/sdk';

export async function patchService(formData: FormData) {
  const ctx = await requireOrg();
  if (!ctx) return { success: false, error: 'Sign in to continue.', status: 401 };

  const id = String(formData.get('service') || '').trim();
  const patch: UpdateServiceRequest = {};

  if (formData.has('replicas')) {
    const replicas = Number(formData.get('replicas'));
    if (!Number.isInteger(replicas) || replicas < 0 || replicas > 100) {
      return { success: false, fieldErrors: { replicas: 'A whole number from 0 to 100' } };
    }
    patch.replicas = replicas;
  }
  if (formData.has('autodeploy')) patch.autodeploy = formData.get('autodeploy') === 'on';

  if (Object.keys(patch).length === 0) return { success: false, error: 'Nothing to change.', status: 422 };

  try {
    if (!assertOwned(ctx.org.id, await fleet.services.get(id))) {
      return { success: false, error: 'No such service.', status: 404 };
    }
    await fleet.services.patch(id, patch);
  } catch (err) {
    return { success: false, error: `Update refused: ${(err as Error).message}`, status: 502 };
  }
  return { success: true, redirect: `/services/${id}` };
}
