/** `wake` on one machine. Ownership first, then exactly one SDK call. */
import { rateLimit } from '@webjsdev/server';
import type { RouteHandlerContext } from '@webjsdev/core';
import { orgOr401, isResponse, jsonBody, notFoundResponse } from '#modules/http/guards.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { assertOwned, fleetErrorResponse } from '#modules/fleet/org-filter.server.ts';

const limited = rateLimit({ window: '1m', max: 30, trustProxy: true, key: 'machine-control:' });

export async function POST(req: Request, { params }: RouteHandlerContext): Promise<Response> {
  return limited(req, async () => {
    const ctx = await orgOr401(req);
    if (isResponse(ctx)) return ctx;
    try {
      if (!assertOwned(ctx.org.id, await fleet.machines.get(params.id))) return notFoundResponse('machine');
      await fleet.machines.wake(params.id);
      return jsonBody({ id: params.id, action: 'wake' });
    } catch (err) {
      return fleetErrorResponse(err);
    }
  });
}
