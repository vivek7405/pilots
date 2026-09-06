'use server';
/**
 * Deploy a service from a named deployment or image.
 *
 * `keep_awake` is the one lifecycle knob the form offers, as a checkbox:
 * ticked, one instance stays running instead of sleeping when idle. It is a
 * patch onto the knobs the previous deployment's instances carry, so an
 * unticked box changes nothing rather than zeroing a policy set elsewhere.
 */
import { requireOrg } from '#modules/auth/session.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { assertOwned } from '#modules/fleet/org-filter.server.ts';
import { backTo } from '#modules/services/utils/back.ts';
import type { DeployRequest } from '@pilots/sdk';

export async function deployService(formData: FormData) {
  const ctx = await requireOrg();
  if (!ctx) return { success: false, error: 'Sign in to continue.', status: 401 };

  const id = String(formData.get('service') || '').trim();
  const body: DeployRequest = {};
  const release = String(formData.get('release') || '').trim();
  const build = String(formData.get('build') || '').trim();
  if (release) body.release = release;
  if (build) body.build = build;
  if (formData.get('keep_awake') === 'on') body.knobs = { min_machines_running: 1 };

  try {
    if (!assertOwned(ctx.org.id, await fleet.services.get(id))) {
      return { success: false, error: 'No such service.', status: 404 };
    }
    await fleet.services.deploy(id, body);
  } catch (err) {
    return { success: false, error: `Deploy refused: ${(err as Error).message}`, status: 502 };
  }
  return { success: true, redirect: backTo(formData, `/services/${id}`, 'deployed') };
}
