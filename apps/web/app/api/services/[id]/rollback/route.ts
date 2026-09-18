/**
 * Roll back to the previous HEALTHY release.
 *
 * The body is empty on purpose: the engine picks the target, because a release
 * that never passed its health gate is not a rollback target and only the
 * engine knows which one did.
 */
import { rateLimit } from '@webjsdev/server';
import type { RouteHandlerContext } from '@webjsdev/core';
import { orgOr401, isResponse, jsonBody, notFoundResponse } from '#modules/http/guards.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { assertOwned, fleetErrorResponse } from '#modules/fleet/org-filter.server.ts';

const limited = rateLimit({ window: '1m', max: 30, trustProxy: true, key: 'services:' });

export async function POST(req: Request, { params }: RouteHandlerContext): Promise<Response> {
  return limited(req, async () => {
    const ctx = await orgOr401(req);
    if (isResponse(ctx)) return ctx;
    try {
      if (!assertOwned(ctx.org.id, await fleet.services.get(params.id))) return notFoundResponse('service');
      return jsonBody(await fleet.services.rollback(params.id));
    } catch (err) {
      return fleetErrorResponse(err);
    }
  });
}
