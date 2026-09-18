/**
 * Restore a checkpoint IN PLACE.
 *
 * The machine keeps its id, its URL and its agent token. A restore that
 * created a machine would mint a new URL, which is a bug rather than a
 * variation: a URL is permanent across suspend, wake, checkpoint, restore,
 * promote, redeploy and host migration.
 */
import { rateLimit } from '@webjsdev/server';
import type { RouteHandlerContext } from '@webjsdev/core';
import { orgOr401, isResponse, jsonBody, notFoundResponse, invalidResponse, readJson, str } from '#modules/http/guards.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { assertOwned, fleetErrorResponse } from '#modules/fleet/org-filter.server.ts';

const limited = rateLimit({ window: '1m', max: 30, trustProxy: true, key: 'machine-control:' });

export async function POST(req: Request, { params }: RouteHandlerContext): Promise<Response> {
  return limited(req, async () => {
    const ctx = await orgOr401(req);
    if (isResponse(ctx)) return ctx;

    const checkpointId = str(await readJson(req), 'checkpoint_id');
    if (!checkpointId) return invalidResponse({ checkpoint_id: 'A checkpoint id is required' });

    try {
      // The MACHINE is what tenancy is checked on: a checkpoint id alone would
      // let an admin-key holder restore into somebody else's machine.
      if (!assertOwned(ctx.org.id, await fleet.machines.get(params.id))) return notFoundResponse('machine');
      const owned = (await fleet.machines.listCheckpoints(params.id)).some((c) => c.id === checkpointId);
      if (!owned) return notFoundResponse('checkpoint');
      return jsonBody(await fleet.checkpoints.restore(checkpointId));
    } catch (err) {
      return fleetErrorResponse(err);
    }
  });
}
