'use server';
/**
 * Connect a repo to a service, from the service page.
 *
 * Both halves are written and they have to agree: the engine's own repo,
 * branch and autodeploy fields, which the webhook handler reads, and the local
 * row the page renders. The installation id lands from the App's own listing,
 * so the page can tell a customer their repo is connected to a service the App
 * cannot see.
 */
import { db } from '#db/connection.server.ts';
import { repoConnections } from '#db/schema.server.ts';
import { requireOrg } from '#modules/auth/session.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { assertOwned } from '#modules/fleet/org-filter.server.ts';
import { installationFor } from '#modules/github/installations.server.ts';
import { isRepoSlug } from '#modules/domains/hostname.ts';

export async function connectRepo(formData: FormData) {
  const ctx = await requireOrg();
  if (!ctx) return { success: false, error: 'Sign in to continue.', status: 401 };

  const id = String(formData.get('service') || '').trim();
  const repo = String(formData.get('repo') || '').trim();
  const branch = String(formData.get('branch') || '').trim() || 'main';
  const autodeploy = formData.get('autodeploy') === 'on';

  if (!isRepoSlug(repo)) return { success: false, fieldErrors: { repo: 'Use owner/name' } };

  try {
    if (!assertOwned(ctx.org.id, await fleet.services.get(id))) {
      return { success: false, error: 'No such service.', status: 404 };
    }
    await fleet.services.patch(id, { repo, branch, autodeploy });
  } catch (err) {
    return { success: false, error: `Connect refused: ${(err as Error).message}`, status: 502 };
  }

  const installation = await installationFor(repo.split('/')[0]);
  await db
    .insert(repoConnections)
    .values({
      orgId: ctx.org.id,
      serviceId: id,
      repo,
      branch,
      autodeploy,
      installationId: installation?.id ?? null,
      connectedBy: ctx.user.id,
    })
    .onConflictDoUpdate({
      target: repoConnections.serviceId,
      set: { repo, branch, autodeploy, installationId: installation?.id ?? null, updatedAt: new Date() },
    });

  return { success: true, redirect: `/services/${id}` };
}
