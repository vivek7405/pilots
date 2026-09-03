'use server';
/** Point a custom domain at one of the org's services. */
import { requireOrg } from '#modules/auth/session.server.ts';
import { fleet, listServices } from '#modules/fleet/client.server.ts';
import { isHostname } from '#modules/domains/hostname.ts';

export async function addDomain(formData: FormData) {
  const ctx = await requireOrg();
  if (!ctx) return { success: false, error: 'Sign in to continue.', status: 401 };

  const hostname = String(formData.get('hostname') || '').trim().toLowerCase();
  const serviceId = String(formData.get('service') || '').trim();
  if (!isHostname(hostname)) {
    return { success: false, fieldErrors: { hostname: 'A hostname, with no scheme, port or path' } };
  }
  if (!serviceId) return { success: false, fieldErrors: { service: 'Choose a service' } };

  try {
    if (!(await listServices(ctx.org.id)).some((s) => s.id === serviceId)) {
      return { success: false, error: 'No such service.', status: 404 };
    }
    await fleet.domains.add({ hostname, service_id: serviceId });
  } catch (err) {
    return { success: false, error: `The fleet refused: ${(err as Error).message}`, status: 502 };
  }
  return { success: true, redirect: '/domains' };
}
