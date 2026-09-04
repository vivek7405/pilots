/** A service's releases, newest first. */
import type { RouteHandlerContext } from '@webjsdev/core';
import { orgOr401, isResponse, jsonBody, notFoundResponse } from '#modules/http/guards.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { assertOwned, fleetErrorResponse } from '#modules/fleet/org-filter.server.ts';

export async function GET(req: Request, { params }: RouteHandlerContext): Promise<Response> {
  const ctx = await orgOr401(req);
  if (isResponse(ctx)) return ctx;
  try {
    if (!assertOwned(ctx.org.id, await fleet.services.get(params.id))) return notFoundResponse('service');
    return jsonBody({ releases: await fleet.services.releases(params.id) });
  } catch (err) {
    return fleetErrorResponse(err);
  }
}
