/**
 * The org's volumes, read-only.
 *
 * Volumes are created by `pilot deploy` and by the API, from a compose file
 * that names them. There is no "new volume" button here because a volume with
 * no service to mount it is a bill with no workload attached.
 */
import { orgOr401, isResponse, jsonBody } from '#modules/http/guards.server.ts';
import { listVolumes } from '#modules/fleet/client.server.ts';
import { fleetErrorResponse } from '#modules/fleet/org-filter.server.ts';

export async function GET(req: Request): Promise<Response> {
  const ctx = await orgOr401(req);
  if (isResponse(ctx)) return ctx;
  try {
    return jsonBody({ volumes: await listVolumes(ctx.org.id) });
  } catch (err) {
    return fleetErrorResponse(err);
  }
}
