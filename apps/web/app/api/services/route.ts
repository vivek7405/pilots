/** The org's services. */
import { orgOr401, isResponse, jsonBody } from '#modules/http/guards.server.ts';
import { listServices } from '#modules/fleet/client.server.ts';
import { fleetErrorResponse } from '#modules/fleet/org-filter.server.ts';

export async function GET(req: Request): Promise<Response> {
  const ctx = await orgOr401(req);
  if (isResponse(ctx)) return ctx;
  try {
    return jsonBody({ services: await listServices(ctx.org.id) });
  } catch (err) {
    return fleetErrorResponse(err);
  }
}
