/**
 * One query against one database, run through a per-request tunnel.
 *
 * Every credential stays on the server. The browser sends a query and gets rows
 * back; it never receives a connection string, and there is no route that would
 * give it one. That is the whole reason this runs here rather than in the page.
 */
import type { RouteHandlerContext } from '@webjsdev/core';
import { orgOr401, isResponse, jsonBody, notFoundResponse, readJson, str } from '#modules/http/guards.server.ts';
import { assertOwned, fleetErrorResponse } from '#modules/fleet/org-filter.server.ts';
import { fleetAs } from '#modules/fleet/client.server.ts';
import { withTunnel } from '#modules/data/tunnel.server.ts';
import { ENGINE_PORT, isEngine, runQuery } from '#modules/data/drivers.server.ts';
import type { Engine } from '#modules/data/drivers.server.ts';

export async function POST(req: Request, { params }: RouteHandlerContext): Promise<Response> {
  const ctx = await orgOr401(req);
  if (isResponse(ctx)) return ctx;

  const raw = await readJson(req);
  const query = str(raw, 'query');
  const target = str(raw, 'target');
  const write = raw.write === true;
  const limit = typeof raw.limit === 'number' ? raw.limit : 0;
  if (query === '') return jsonBody({ error: 'nothing to run' }, 400);

  const client = fleetAs(ctx.org.id);
  try {
    const service = await client.services.get(params.id);
    if (!assertOwned(ctx.org.id, service)) return notFoundResponse('service');
    const engine = service.labels?.['pilot.engine'];
    if (!isEngine(engine)) {
      return jsonBody({ error: `${service.name} is not a database` }, 400);
    }

    // A RUNNING replica, or there is nothing to query. Reported as its own
    // message rather than as a connection failure: "the database is asleep" and
    // "the database refused you" need different things done about them.
    const machines = await client.machines.list();
    const instance = machines.find((m) => m.service_id === service.id && m.state === 'running');
    if (!instance) {
      return jsonBody(
        { error: `${service.name} has no running instance to query` },
        409,
      );
    }

    // The credentials, read once, held for the length of this request. The
    // deliberate route, called deliberately: see hostd's serviceenv.go.
    const env = await client.services.env(service.id);
    const url = connectionURL(engine, { ...env.env, ...env.secret_env });
    if (!url) {
      return jsonBody(
        {
          error: `no connection string on ${service.name}; the Data view reads the one \`pilot add\` set`,
        },
        409,
      );
    }

    const result = await withTunnel(ctx.org.id, instance.id, ENGINE_PORT[engine], (tunnel) =>
      runQuery({
        engine,
        tunnel,
        url,
        query,
        ...(target ? { target } : {}),
        ...(write ? { write: true } : {}),
        ...(limit ? { limit } : {}),
      }),
    );
    return jsonBody(result);
  } catch (err) {
    // An engine's own error is the useful one and is passed through: "relation
    // does not exist" tells somebody what to do, where "query failed" does not.
    if (err instanceof Error && !('status' in err)) {
      return jsonBody({ error: err.message }, 400);
    }
    return fleetErrorResponse(err);
  }
}

/**
 * The connection string to use, preferring the DIRECT address.
 *
 * A browser query is an interactive session, which is exactly the connection
 * that should not go through a transaction pooler: temporary tables, session
 * advisory locks and LISTEN all stop working, and a query that fails for that
 * reason says nothing about the reason.
 */
function connectionURL(engine: Engine, env: Record<string, string>): string | undefined {
  const names: Record<Engine, string[]> = {
    postgres: ['DATABASE_URL_DIRECT', 'DATABASE_URL'],
    mysql: ['MYSQL_URL', 'DATABASE_URL'],
    redis: ['REDIS_URL'],
    mongo: ['MONGO_URL', 'MONGODB_URI'],
  };
  for (const name of names[engine]) {
    const value = env[name];
    if (value) return value;
  }
  return undefined;
}
