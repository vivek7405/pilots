/** One machine: read it, or destroy it. A foreign id is a 404, never a 403. */
import { rateLimit } from '@webjsdev/server';
import type { RouteHandlerContext } from '@webjsdev/core';
import { orgOr401, isResponse, jsonBody, notFoundResponse } from '#modules/http/guards.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { assertOwned, fleetErrorResponse } from '#modules/fleet/org-filter.server.ts';

const limited = rateLimit({ window: '1m', max: 30, trustProxy: true, key: 'machine-control:' });

export async function GET(req: Request, { params }: RouteHandlerContext): Promise<Response> {
  const ctx = await orgOr401(req);
  if (isResponse(ctx)) return ctx;
  try {
    const machine = assertOwned(ctx.org.id, await fleet.machines.get(params.id));
    return machine ? jsonBody(machine) : notFoundResponse('machine');
  } catch (err) {
    return fleetErrorResponse(err);
  }
}

export async function DELETE(req: Request, { params }: RouteHandlerContext): Promise<Response> {
  return limited(req, async () => {
    const ctx = await orgOr401(req);
    if (isResponse(ctx)) return ctx;
    try {
      // Ownership is checked with a read BEFORE the destroy: this app holds an
      // admin key, so an unchecked id would delete another tenant's machine.
      if (!assertOwned(ctx.org.id, await fleet.machines.get(params.id))) return notFoundResponse('machine');
      await fleet.machines.destroy(params.id);
      return jsonBody({ destroyed: params.id });
    } catch (err) {
      return fleetErrorResponse(err);
    }
  });
}
