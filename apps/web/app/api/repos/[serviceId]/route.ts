/**
 * Connect a repo to a service, or disconnect it.
 *
 * The engine is what acts on a push; this route only records the intent. It
 * writes both halves and they have to agree: `PATCH /v1/services/{id}` is what
 * the webhook handler reads to decide whether to build, and the local
 * `repo_connections` row is what the service page renders and what carries the
 * installation id.
 *
 * A disconnect patches the service's repo fields EMPTY before deleting the
 * local row, in that order. The reverse would leave a service that still
 * autodeploys with nothing in the dashboard saying so.
 */

import { eq } from 'drizzle-orm';
import { rateLimit } from '@webjsdev/server';
import type { RouteHandlerContext } from '@webjsdev/core';
import { db } from '#db/connection.server.ts';
import { repoConnections } from '#db/schema.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { assertOwned, fleetErrorResponse } from '#modules/fleet/org-filter.server.ts';
import { claimRepo } from '#modules/github/claim.server.ts';
import { isRepoSlug } from '#modules/domains/hostname.ts';
import { invalidResponse, isResponse, jsonBody, notFoundResponse, orgOr401, readJson, str } from '#modules/http/guards.server.ts';

const limited = rateLimit({ window: '1m', max: 10, trustProxy: true, key: 'repos:' });

export async function GET(req: Request, { params }: RouteHandlerContext): Promise<Response> {
  const ctx = await orgOr401(req);
  if (isResponse(ctx)) return ctx;

  try {
    if (!assertOwned(ctx.org.id, await fleet.services.get(params.serviceId))) return notFoundResponse('service');
  } catch (err) {
    return fleetErrorResponse(err);
  }

  const row = await db.select().from(repoConnections).where(eq(repoConnections.serviceId, params.serviceId)).get();
  if (!row) return jsonBody({ connected: false });
  return jsonBody({
    connected: true,
    repo: row.repo,
    branch: row.branch,
    autodeploy: row.autodeploy,
    installation_id: row.installationId,
  });
}

export async function PUT(req: Request, { params }: RouteHandlerContext): Promise<Response> {
  return limited(req, async () => {
    const ctx = await orgOr401(req);
    if (isResponse(ctx)) return ctx;

    const raw = await readJson(req);
    const repo = str(raw, 'repo');
    const branch = str(raw, 'branch') || 'main';
    const autodeploy = raw.autodeploy === undefined ? true : Boolean(raw.autodeploy);

    const fieldErrors: Record<string, string> = {};
    if (!isRepoSlug(repo)) fieldErrors.repo = 'Use owner/name';
    if (!branch) fieldErrors.branch = 'A branch is required';
    if (Object.keys(fieldErrors).length) return invalidResponse(fieldErrors);

    // The fleet CLAIM first, then the engine's fields, then the local row.
    //
    // hostd keeps its own `repo_links` row per (org, repository) and reads it
    // before it will fetch a repository by name. Every surface in this app that
    // connects a repository writes it through claimRepo -- this route, the
    // action behind the service page's form, and the deploy wizard -- because a
    // surface that skips it leaves the page saying "connected" while the org's
    // own key is refused that repository by `pilot deploy` and by every agent.
    //
    // installation_id stays null, and the connect still succeeds, for an owner
    // the App is not installed on: that is this route's deliberate behaviour
    // and the page renders its install link off exactly that null. No CLAIM is
    // written in that case, because a claim is write-once with no disconnect
    // and must never name an account the fleet was not given.
    let claim;
    try {
      if (!assertOwned(ctx.org.id, await fleet.services.get(params.serviceId))) return notFoundResponse('service');
      claim = await claimRepo(ctx.org.id, repo);
      await fleet.services.patch(params.serviceId, { repo, branch, autodeploy });
    } catch (err) {
      return fleetErrorResponse(err);
    }
    await db
      .insert(repoConnections)
      .values({
        orgId: ctx.org.id,
        serviceId: params.serviceId,
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

    return jsonBody({
      connected: true,
      repo,
      branch,
      autodeploy,
      installation_id: claim.installationId,
    });
  });
}

export async function DELETE(req: Request, { params }: RouteHandlerContext): Promise<Response> {
  return limited(req, async () => {
    const ctx = await orgOr401(req);
    if (isResponse(ctx)) return ctx;

    try {
      if (!assertOwned(ctx.org.id, await fleet.services.get(params.serviceId))) return notFoundResponse('service');
      // The engine first: a service left autodeploying with no row here would
      // keep building on every push with nothing in the UI explaining it.
      await fleet.services.patch(params.serviceId, { repo: '', branch: '', autodeploy: false });
    } catch (err) {
      return fleetErrorResponse(err);
    }

    await db.delete(repoConnections).where(eq(repoConnections.serviceId, params.serviceId));
    return jsonBody({ connected: false });
  });
}
