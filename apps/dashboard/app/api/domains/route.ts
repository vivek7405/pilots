/**
 * Custom domains.
 *
 * `domains.list()` is fleet-wide, so it is narrowed here against the org's own
 * services rather than trusted as it arrives: this app's key is an admin key,
 * which sees every domain on the fleet.
 */
import { rateLimit } from '@webjsdev/server';
import { orgOr401, isResponse, jsonBody, invalidResponse, notFoundResponse, readJson, str } from '#modules/http/guards.server.ts';
import { fleet, listServices } from '#modules/fleet/client.server.ts';
import { fleetErrorResponse } from '#modules/fleet/org-filter.server.ts';
import { isHostname } from '#modules/domains/hostname.ts';

const limited = rateLimit({ window: '1m', max: 30, trustProxy: true, key: 'domains:' });

export async function GET(req: Request): Promise<Response> {
  const ctx = await orgOr401(req);
  if (isResponse(ctx)) return ctx;
  try {
    const mine = new Set((await listServices(ctx.org.id)).map((s) => s.id));
    const domains = (await fleet.domains.list()).filter((d) => mine.has(d.service_id));
    return jsonBody({ domains });
  } catch (err) {
    return fleetErrorResponse(err);
  }
}

export async function POST(req: Request): Promise<Response> {
  return limited(req, async () => {
    const ctx = await orgOr401(req);
    if (isResponse(ctx)) return ctx;

    const raw = await readJson(req);
    const hostname = str(raw, 'hostname').toLowerCase();
    const serviceId = str(raw, 'service_id');
    const fieldErrors: Record<string, string> = {};
    if (!isHostname(hostname)) fieldErrors.hostname = 'Enter a hostname, with no scheme, port or path';
    if (!serviceId) fieldErrors.service_id = 'Choose a service';
    if (Object.keys(fieldErrors).length) return invalidResponse(fieldErrors);

    try {
      const mine = (await listServices(ctx.org.id)).some((s) => s.id === serviceId);
      if (!mine) return notFoundResponse('service');
      return jsonBody(await fleet.domains.add({ hostname, service_id: serviceId }));
    } catch (err) {
      return fleetErrorResponse(err);
    }
  });
}
