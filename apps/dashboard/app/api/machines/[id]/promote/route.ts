/**
 * Turn a sandbox into a durable service.
 *
 * The machine's URL does not change: promote is a lifecycle change on one
 * machine, not a new object, and a URL that moved would break every link a
 * customer already handed out.
 */
import { rateLimit } from '@webjsdev/server';
import type { RouteHandlerContext } from '@webjsdev/core';
import type { PromoteRequest } from '@pilots/sdk';
import { orgOr401, isResponse, jsonBody, notFoundResponse, invalidResponse, readJson, str } from '#modules/http/guards.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { assertOwned, fleetErrorResponse } from '#modules/fleet/org-filter.server.ts';
import { isHostname } from '#modules/domains/hostname.ts';

const limited = rateLimit({ window: '1m', max: 30, trustProxy: true, key: 'machine-control:' });

export async function POST(req: Request, { params }: RouteHandlerContext): Promise<Response> {
  return limited(req, async () => {
    const ctx = await orgOr401(req);
    if (isResponse(ctx)) return ctx;

    const raw = await readJson(req);
    const fieldErrors: Record<string, string> = {};
    const body: PromoteRequest = {};

    const domain = str(raw, 'custom_domain');
    if (domain) {
      if (!isHostname(domain)) fieldErrors.custom_domain = 'Enter a hostname, with no scheme or path';
      else body.custom_domain = domain;
    }
    if (raw.replicas !== undefined) {
      const replicas = Number(raw.replicas);
      if (!Number.isInteger(replicas) || replicas < 1 || replicas > 100) {
        fieldErrors.replicas = 'Replicas must be a whole number from 1 to 100';
      } else body.replicas = replicas;
    }
    if (Object.keys(fieldErrors).length) return invalidResponse(fieldErrors);

    try {
      if (!assertOwned(ctx.org.id, await fleet.machines.get(params.id))) return notFoundResponse('machine');
      return jsonBody(await fleet.machines.promote(params.id, body));
    } catch (err) {
      return fleetErrorResponse(err);
    }
  });
}
