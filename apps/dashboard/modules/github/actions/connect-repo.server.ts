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
import { claimRepo } from '#modules/github/claim.server.ts';
import { isRepoSlug } from '#modules/domains/hostname.ts';
import { backTo } from '#modules/services/utils/back.ts';

export async function connectRepo(formData: FormData) {
  const ctx = await requireOrg();
  if (!ctx) return { success: false, error: 'Sign in to continue.', status: 401 };

  const id = String(formData.get('service') || '').trim();
  const repo = String(formData.get('repo') || '').trim();
  const branch = String(formData.get('branch') || '').trim() || 'main';
  const autodeploy = formData.get('autodeploy') === 'on';

  if (!isRepoSlug(repo)) return { success: false, fieldErrors: { repo: 'Use owner/name' } };

  // The fleet CLAIM first, then the engine's fields, then the local row.
  //
  // Three surfaces in this app connect a repository -- this action, the JSON
  // route behind it, and the deploy wizard -- and every one of them has to
  // write hostd's `repo_links` row, or the same product action leaves two
  // different fleet states: the page says "connected", the webhook autodeploys,
  // and the org's OWN key is refused that repository by `pilot deploy` and by
  // every agent. That is what claimRepo is for, and it is also where the
  // ownership check lives.
  //
  // An owner the App is not installed on connects ANYWAY here, with no claim
  // and a null installation id, because that is this surface's existing and
  // deliberate behaviour: the page renders the install link off exactly that
  // null. What it must not do is mint a permanent claim on an account the
  // fleet was never given -- claims are write-once and there is no disconnect
  // -- so the claim waits until the App is installed and the person connects
  // again.
  let claim;
  try {
    if (!assertOwned(ctx.org.id, await fleet.services.get(id))) {
      return { success: false, error: 'No such service.', status: 404 };
    }
    claim = await claimRepo(ctx.org.id, repo);
    await fleet.services.patch(id, { repo, branch, autodeploy });
  } catch (err) {
    return { success: false, error: `Connect refused: ${(err as Error).message}`, status: 502 };
  }

  await db
    .insert(repoConnections)
    .values({
      orgId: ctx.org.id,
      serviceId: id,
      repo,
      branch,
      autodeploy,
      installationId: claim.installationId,
      connectedBy: ctx.user.id,
    })
    .onConflictDoUpdate({
      target: repoConnections.serviceId,
      set: { repo, branch, autodeploy, installationId: claim.installationId, updatedAt: new Date() },
    });

  return { success: true, redirect: backTo(formData, `/services/${id}`, 'connected') };
}
