/** Deploy a service, from a named release or a named build. */
import { rateLimit } from '@webjsdev/server';
import type { RouteHandlerContext } from '@webjsdev/core';
import type { DeployRequest } from '@pilots/sdk';
import { orgOr401, isResponse, jsonBody, notFoundResponse, readJson, str } from '#modules/http/guards.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { assertOwned, fleetErrorResponse } from '#modules/fleet/org-filter.server.ts';

const limited = rateLimit({ window: '1m', max: 30, trustProxy: true, key: 'services:' });

export async function POST(req: Request, { params }: RouteHandlerContext): Promise<Response> {
  return limited(req, async () => {
    const ctx = await orgOr401(req);
    if (isResponse(ctx)) return ctx;

    const raw = await readJson(req);
    const body: DeployRequest = {};
    const release = str(raw, 'release');
    const build = str(raw, 'build');
    if (release) body.release = release;
    if (build) body.build = build;

    try {
      if (!assertOwned(ctx.org.id, await fleet.services.get(params.id))) return notFoundResponse('service');
      return jsonBody(await fleet.services.deploy(params.id, body));
    } catch (err) {
      return fleetErrorResponse(err);
    }
  });
}
