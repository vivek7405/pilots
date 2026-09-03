/**
 * The fleet's hosts.
 *
 * This is the ONE feed every signed-in user is entitled to see identically, so
 * it is the one place `broadcast` is correct: a host's liveness, free CPU and
 * free memory belong to no tenant. Every per-org feed keeps its own subscriber
 * set instead (see modules/machines/live.server.ts).
 */
import { orgOr401, isResponse, jsonBody } from '#modules/http/guards.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { requireOrg } from '#modules/auth/session.server.ts';
import { fleetErrorResponse } from '#modules/fleet/org-filter.server.ts';

export async function GET(req: Request): Promise<Response> {
  const ctx = await orgOr401(req);
  if (isResponse(ctx)) return ctx;
  try {
    return jsonBody({ hosts: await fleet.hosts.list() });
  } catch (err) {
    return fleetErrorResponse(err);
  }
}

/**
 * The live hosts strip.
 *
 * There is nothing to send from here: the usage poller `broadcast`s to this
 * path on every tick, and `broadcast` reaches every socket the framework holds
 * for it. What this handler owns is the gate -- a `WS` export gets no
 * middleware, so an anonymous socket would otherwise sit on the fleet's host
 * inventory -- and the opening snapshot, so a viewer sees hosts before the
 * first tick rather than up to a minute later.
 */
export async function WS(
  ws: { send(data: string): void; close(code?: number, reason?: string): void },
  req: Request,
): Promise<void> {
  const ctx = await requireOrg(req);
  if (!ctx) {
    ws.close(4401, 'unauthorized');
    return;
  }
  try {
    ws.send(JSON.stringify({ hosts: await fleet.hosts.list() }));
  } catch {
    ws.close(1011, 'fleet unavailable');
  }
}
