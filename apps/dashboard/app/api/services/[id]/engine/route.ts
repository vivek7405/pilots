/**
 * What the database engine says about itself.
 *
 * Read by running the engine's own client inside its machine, which is the same
 * thing `pilot metrics` does and the same command string, mirrored in the SDK
 * with a drift test over it. No exporter is installed and nothing extra runs
 * beside anybody's database.
 */
import type { RouteHandlerContext } from '@webjsdev/core';
import { metricsCommand, parseEngineMetrics } from '@pilots/sdk';
import { fleetAs } from '#modules/fleet/client.server.ts';
import { assertOwned, fleetErrorResponse } from '#modules/fleet/org-filter.server.ts';
import { isResponse, jsonBody, notFoundResponse, orgOr401 } from '#modules/http/guards.server.ts';

export async function GET(req: Request, { params }: RouteHandlerContext): Promise<Response> {
  const ctx = await orgOr401(req);
  if (isResponse(ctx)) return ctx;

  const client = fleetAs(ctx.org.id);
  try {
    const service = await client.services.get(params.id);
    if (!assertOwned(ctx.org.id, service)) return notFoundResponse('service');

    const engine = service.labels?.['pilot.engine'] ?? '';
    const command = metricsCommand(engine);
    if (command === '') {
      return jsonBody({ error: `${service.name} is not a database` }, 400);
    }

    const machines = await client.machines.list();
    const instance = machines.find((m) => m.service_id === service.id && m.state === 'running');
    if (!instance) {
      return jsonBody({ error: `${service.name} has no running instance to ask` }, 409);
    }

    const res = await client.machines.exec(instance.id, { cmd: command, user: 'root' });
    if (res.exit_code !== 0) {
      // The engine's own stderr, because it says which of the many reasons
      // this is: still starting, wrong password, out of connections.
      return jsonBody({ error: res.stderr || 'the engine refused the query' }, 502);
    }
    return jsonBody(parseEngineMetrics(engine, res.stdout));
  } catch (err) {
    return fleetErrorResponse(err);
  }
}
