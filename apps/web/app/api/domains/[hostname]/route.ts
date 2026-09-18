/** Remove a custom domain, after proving it points at one of this org's services. */
import { rateLimit } from '@webjsdev/server';
import type { RouteHandlerContext } from '@webjsdev/core';
import { orgOr401, isResponse, jsonBody, notFoundResponse } from '#modules/http/guards.server.ts';
import { fleet, listServices } from '#modules/fleet/client.server.ts';
import { fleetErrorResponse } from '#modules/fleet/org-filter.server.ts';

const limited = rateLimit({ window: '1m', max: 30, trustProxy: true, key: 'domains:' });

export async function DELETE(req: Request, { params }: RouteHandlerContext): Promise<Response> {
  return limited(req, async () => {
    const ctx = await orgOr401(req);
    if (isResponse(ctx)) return ctx;
    const hostname = params.hostname.toLowerCase();
    try {
      const mine = new Set((await listServices(ctx.org.id)).map((s) => s.id));
      const domain = (await fleet.domains.list()).find((d) => d.hostname.toLowerCase() === hostname);
      if (!domain || !mine.has(domain.service_id)) return notFoundResponse('domain');
      await fleet.domains.remove(domain.hostname);
      return jsonBody({ hostname: domain.hostname, removed: true });
    } catch (err) {
      return fleetErrorResponse(err);
    }
  });
}
