/** Checkpoints on one machine: create one, or list what exists. */
import { rateLimit } from '@webjsdev/server';
import type { RouteHandlerContext } from '@webjsdev/core';
import { orgOr401, isResponse, jsonBody, notFoundResponse, readJson, str } from '#modules/http/guards.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { assertOwned, fleetErrorResponse } from '#modules/fleet/org-filter.server.ts';

const limited = rateLimit({ window: '1m', max: 30, trustProxy: true, key: 'machine-control:' });

export async function GET(req: Request, { params }: RouteHandlerContext): Promise<Response> {
  const ctx = await orgOr401(req);
  if (isResponse(ctx)) return ctx;
  try {
    if (!assertOwned(ctx.org.id, await fleet.machines.get(params.id))) return notFoundResponse('machine');
    return jsonBody({ checkpoints: await fleet.machines.listCheckpoints(params.id) });
  } catch (err) {
    return fleetErrorResponse(err);
  }
}

export async function POST(req: Request, { params }: RouteHandlerContext): Promise<Response> {
  return limited(req, async () => {
    const ctx = await orgOr401(req);
    if (isResponse(ctx)) return ctx;
    const comment = str(await readJson(req), 'comment').slice(0, 200);
    try {
      if (!assertOwned(ctx.org.id, await fleet.machines.get(params.id))) return notFoundResponse('machine');
      return jsonBody(await fleet.machines.checkpoint(params.id, comment ? { comment } : {}));
    } catch (err) {
      return fleetErrorResponse(err);
    }
  });
}
