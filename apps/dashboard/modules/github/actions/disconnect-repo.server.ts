'use server';
/**
 * Disconnect a repo.
 *
 * The engine's fields are emptied BEFORE the row is dropped. The reverse would
 * leave a service still autodeploying on every push with nothing in the
 * dashboard saying why.
 */
import { eq } from 'drizzle-orm';
import { db } from '#db/connection.server.ts';
import { repoConnections } from '#db/schema.server.ts';
import { requireOrg } from '#modules/auth/session.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { assertOwned } from '#modules/fleet/org-filter.server.ts';

export async function disconnectRepo(formData: FormData) {
  const ctx = await requireOrg();
  if (!ctx) return { success: false, error: 'Sign in to continue.', status: 401 };

  const id = String(formData.get('service') || '').trim();
  try {
    if (!assertOwned(ctx.org.id, await fleet.services.get(id))) {
      return { success: false, error: 'No such service.', status: 404 };
    }
    await fleet.services.patch(id, { repo: '', branch: '', autodeploy: false });
  } catch (err) {
    return { success: false, error: `Disconnect refused: ${(err as Error).message}`, status: 502 };
  }

  await db.delete(repoConnections).where(eq(repoConnections.serviceId, id));
  return { success: true, redirect: `/services/${id}` };
}
