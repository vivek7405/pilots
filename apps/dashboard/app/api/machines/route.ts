/**
 * The org's machines: a list, and a live feed of the same rows.
 *
 * The `WS` export gets NO middleware -- the framework runs none for a socket
 * upgrade -- so it authenticates itself and closes 4401 when it cannot.
 */
import { orgOr401, isResponse, jsonBody } from '#modules/http/guards.server.ts';
import { requireOrg } from '#modules/auth/session.server.ts';
import { listMachines } from '#modules/fleet/client.server.ts';
import { fleetErrorResponse } from '#modules/fleet/org-filter.server.ts';
import { subscribe, unsubscribe } from '#modules/machines/live.server.ts';
import type { LiveSocket } from '#modules/machines/live.server.ts';

export async function GET(req: Request): Promise<Response> {
  const ctx = await orgOr401(req);
  if (isResponse(ctx)) return ctx;
  try {
    return jsonBody({ machines: await listMachines(ctx.org.id) });
  } catch (err) {
    return fleetErrorResponse(err);
  }
}

export async function WS(ws: LiveSocket & { on(event: string, fn: () => void): void }, req: Request): Promise<void> {
  const ctx = await requireOrg(req);
  if (!ctx) {
    ws.close(4401, 'unauthorized');
    return;
  }
  const orgId = ctx.org.id;
  ws.on('close', () => unsubscribe(orgId, ws));
  try {
    await subscribe(orgId, ws);
  } catch {
    ws.close(1011, 'fleet unavailable');
  }
}
