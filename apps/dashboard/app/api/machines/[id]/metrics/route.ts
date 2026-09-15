/**
 * What one instance is using. A foreign id is a 404, never a 403.
 *
 * Re-served rather than proxied to the fleet from the browser, for the reason
 * every other route here is: the browser holds no fleet credential, and the one
 * it would need to read this is one that could read everything.
 */
import type { RouteHandlerContext } from '@webjsdev/core';
import { orgOr401, isResponse, jsonBody, notFoundResponse } from '#modules/http/guards.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { assertOwned, fleetErrorResponse } from '#modules/fleet/org-filter.server.ts';

export async function GET(req: Request, { params }: RouteHandlerContext): Promise<Response> {
  const ctx = await orgOr401(req);
  if (isResponse(ctx)) return ctx;
  try {
    // Ownership is checked against the MACHINE, then the numbers are fetched.
    // The other way round would answer with a foreign machine's usage before
    // deciding whether the caller may see it.
    const machine = assertOwned(ctx.org.id, await fleet.machines.get(params.id));
    if (!machine) return notFoundResponse('machine');
    return jsonBody(await fleet.machines.metrics(params.id));
  } catch (err) {
    return fleetErrorResponse(err);
  }
}
