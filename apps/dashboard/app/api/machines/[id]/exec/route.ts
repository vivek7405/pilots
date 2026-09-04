/**
 * The exec console: one command per socket, over the engine's exec stream.
 *
 * This is NOT a PTY. hostd exposes `GET /v1/machines/{id}/exec/stream` and
 * nothing that proxies the guest agent's terminal frames, so a real terminal
 * would mean a second stream protocol and an engine route. One command per
 * submit is the same path the reference workload runs.
 *
 * `stdin` is always false. A process holding an open stdin it never reads
 * hangs, and this console has no way for a viewer to close it, so an interactive
 * command would look like a console that stopped answering.
 *
 * A `WS` export gets no middleware -- the framework runs none for an upgrade --
 * so this authenticates itself and closes 4401 when it cannot.
 */

import type { RouteHandlerContext } from '@webjsdev/core';
import { requireOrg } from '#modules/auth/session.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { assertOwned } from '#modules/fleet/org-filter.server.ts';

/** The socket surface used here; the framework's `ws` satisfies it. */
export interface ExecSocket {
  send(data: string): void;
  close(code?: number, reason?: string): void;
  on(event: string, fn: (data?: unknown) => void): void;
}

/** The one message a client sends: the argv to run, and an optional cwd. */
interface ExecRequestMessage {
  cmd?: unknown;
  dir?: unknown;
}

export async function WS(ws: ExecSocket, req: Request, { params }: RouteHandlerContext): Promise<void> {
  const ctx = await requireOrg(req);
  if (!ctx) {
    ws.close(4401, 'unauthorized');
    return;
  }

  try {
    if (!assertOwned(ctx.org.id, await fleet.machines.get(params.id))) {
      ws.close(4404, 'not found');
      return;
    }
  } catch {
    ws.close(1011, 'fleet unavailable');
    return;
  }

  let running = false;
  ws.on('message', (data) => {
    if (running) return;
    const argv = parseArgv(data);
    if (!argv) {
      ws.send(JSON.stringify({ type: 'error', message: 'send {"cmd": ["sh", "-c", "..."]}' }));
      return;
    }
    running = true;
    void run(ws, params.id, argv.cmd, argv.dir);
  });
}

async function run(ws: ExecSocket, machineId: string, cmd: string[], dir?: string): Promise<void> {
  try {
    const stream = fleet.machines.execStream(machineId, cmd, {
      user: 'sprite',
      stdin: false,
      ...(dir ? { cwd: dir } : {}),
    });

    stream.stdout.on('data', (chunk: Buffer) => {
      ws.send(JSON.stringify({ type: 'stdout', data: chunk.toString('base64') }));
    });
    stream.stderr.on('data', (chunk: Buffer) => {
      ws.send(JSON.stringify({ type: 'stderr', data: chunk.toString('base64') }));
    });

    // The SDK ends both output streams BEFORE the exit resolves, so every
    // output frame is already sent by the time this line runs.
    const code = await stream.wait();
    ws.send(JSON.stringify({ type: 'exit', code }));
  } catch (err) {
    ws.send(JSON.stringify({ type: 'error', message: (err as Error).message }));
  } finally {
    ws.close(1000, 'done');
  }
}

/** Validates the one client message. Anything else is refused, not guessed. */
function parseArgv(data: unknown): { cmd: string[]; dir?: string } | null {
  let parsed: ExecRequestMessage;
  try {
    parsed = typeof data === 'string' ? (JSON.parse(data) as ExecRequestMessage) : (data as ExecRequestMessage);
  } catch {
    return null;
  }
  const cmd = parsed?.cmd;
  if (!Array.isArray(cmd) || cmd.length === 0 || cmd.length > 64) return null;
  if (!cmd.every((a) => typeof a === 'string' && a.length <= 4096)) return null;
  const dir = typeof parsed.dir === 'string' && parsed.dir.trim() ? parsed.dir.trim() : undefined;
  return { cmd: cmd as string[], ...(dir ? { dir } : {}) };
}
