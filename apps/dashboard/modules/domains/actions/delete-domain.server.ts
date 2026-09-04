'use server';
/** Remove a custom domain, after proving it points at one of this org's services. */
import { requireOrg } from '#modules/auth/session.server.ts';
import { fleet, listServices } from '#modules/fleet/client.server.ts';

export async function deleteDomain(formData: FormData) {
  const ctx = await requireOrg();
  if (!ctx) return { success: false, error: 'Sign in to continue.', status: 401 };

  const hostname = String(formData.get('hostname') || '').trim().toLowerCase();
  try {
    const mine = new Set((await listServices(ctx.org.id)).map((s) => s.id));
    const domain = (await fleet.domains.list()).find((d) => d.hostname.toLowerCase() === hostname);
    if (!domain || !mine.has(domain.service_id)) return { success: false, error: 'No such domain.', status: 404 };
    await fleet.domains.remove(domain.hostname);
  } catch (err) {
    return { success: false, error: `The fleet refused: ${(err as Error).message}`, status: 502 };
  }
  return { success: true, redirect: '/domains' };
}
