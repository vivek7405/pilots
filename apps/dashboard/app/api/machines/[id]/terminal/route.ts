/**
 * An interactive terminal, over the exec stream's tty mode.
 *
 * The exec console beside this one is a different thing and stays: it runs one
 * command per socket with stdin off, which is what an agent's one-shot needs.
 * This one holds a shell open on a pseudo-terminal, which is what `tmux`,
 * `vim` and a person need and what three pipes cannot give.
 *
 * Nothing new is routed to get here. hostd's exec stream grew `tty`, `rows`
 * and `cols`, so this is the SAME endpoint the console uses with a different
 * query. The guest agent's own `/terminal` handler is not involved.
 *
 * The JSON vocabulary is deliberately the guest agent's own terminal frame
 * shape (`{type, data, cols, rows, code}`), so a reader following this path
 * from the browser to the guest meets one message format rather than two.
 *
 * A `WS` export gets no middleware -- the framework runs none for an upgrade --
 * so this authenticates itself and closes 4401 when it cannot.
 */

import type { RouteHandlerContext } from '@webjsdev/core';
import { requireOrg } from '#modules/auth/session.server.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { assertOwned } from '#modules/fleet/org-filter.server.ts';
import { socketJson } from '#lib/socket-text.server.ts';

/** The socket surface used here; the framework's `ws` satisfies it. */
export interface TerminalSocket {
  send(data: string): void;
  close(code?: number, reason?: string): void;
  on(event: string, fn: (data?: unknown) => void): void;
}

/** What the client sends. `open` must come first and may come only once. */
interface ClientMessage {
  type?: unknown;
  data?: unknown;
  cols?: unknown;
  rows?: unknown;
}

/**
 * The login shell, chosen in the guest rather than here.
 *
 * `bash -l` where it exists and `sh -l` otherwise: a built image may be alpine
 * or distroless, and asking for a shell that is not there would end the
 * session with a start failure instead of a prompt.
 */
/**
 * ABSOLUTE path, not a bare `sh`. The guest agent execs the argv it is given
 * without a PATH search, so `sh` fails to start on an image whose environment
 * the agent does not inherit: the socket opens, no shell ever runs, and the
 * terminal shows a blinking cursor that swallows every keystroke. `/bin/sh`
 * exists in every image pilots can build. The interactive shell is still
 * chosen INSIDE the guest, so an image without bash still gets one.
 */
const SHELL = ['/bin/sh', '-c', 'command -v bash >/dev/null 2>&1 && exec bash -l || exec /bin/sh -l'];

/** A window size the guest will accept. Out of range closes the stream. */
function dimension(value: unknown, fallback: number): number {
  const n = typeof value === 'number' ? value : Number(value);
  if (!Number.isInteger(n) || n < 1 || n > 65_535) return fallback;
  return n;
}

export async function WS(ws: TerminalSocket, req: Request, { params }: RouteHandlerContext): Promise<void> {
  // The stream is not opened until the client has said how big its window is.
  // A shell reads its window size at startup, so opening one at 24 by 80 and
  // resizing a moment later makes the first prompt redraw visibly.
  let stream: ReturnType<typeof fleet.machines.execStream> | null = null;
  let sandbox = false;
  let closed = false;

  // Frames that arrived before the handler finished authenticating. The
  // listener below is attached SYNCHRONOUSLY, so nothing the client sends is
  // ever dropped; anything early waits here until `ready` runs.
  const early: unknown[] = [];
  let ready = false;

  const handle = (data: unknown): void => {
    const message = parse(data);
    if (!message) return;

    if (message.type === 'open') {
      if (stream || closed) return; // `open` is once; a second one is ignored, not obeyed
      stream = start(ws, params.id, sandbox, dimension(message.rows, 24), dimension(message.cols, 80));
      return;
    }
    if (!stream) return;

    if (message.type === 'data' && typeof message.data === 'string') {
      try {
        stream.writeStdin(Buffer.from(message.data, 'base64'));
      } catch {
        // A stream that has already exited is not an error worth closing over;
        // the exit frame has already told the client what happened.
      }
      return;
    }
    if (message.type === 'resize') {
      try {
        stream.resize(dimension(message.cols, 80), dimension(message.rows, 24));
      } catch {
        // Same: a resize after the shell exited changes nothing.
      }
    }
  };

  // ATTACHED BEFORE THE FIRST `await`, and this ordering is the whole bug fix.
  // The browser sends `open` from its own `open` event, which fires the instant
  // the upgrade completes. Authenticating first costs a database read and a
  // call to hostd, tens of milliseconds during which `ws` is already flowing
  // and buffers nothing: the `open` frame was delivered to a socket with no
  // listener and vanished. The terminal then sat on "Connected" for ever with a
  // cursor that swallowed every keystroke, because the shell was never started.
  // Registering here and queueing into `early` makes the handler's own latency
  // invisible to the client.
  ws.on('message', (data) => {
    if (!ready) {
      // A ceiling, so a client that floods before auth completes cannot use the
      // queue as free memory. Twenty frames is a window size and a few
      // keystrokes; beyond that the connection is not a terminal session.
      if (early.length < 20) early.push(data);
      return;
    }
    handle(data);
  });

  ws.on('close', () => {
    // Killing the stream closes the socket to the guest, whose context cancel
    // kills the shell. Without this a closed browser tab leaves a shell
    // running and a machine that never goes idle.
    closed = true;
    stream?.kill();
    stream = null;
  });

  const ctx = await requireOrg(req);
  if (!ctx) {
    ws.close(4401, 'unauthorized');
    return;
  }

  // The user is resolved per machine, never hardcoded. A sandbox from the
  // golden rootfs has `sprite`; a service replica built from someone's
  // Dockerfile very often does not, and asking for it fails closed with
  // "user does not exist". A replica asks for no user and the guest agent
  // runs the image's own, which is what docker exec would do.
  try {
    const machine = await fleet.machines.get(params.id);
    if (!assertOwned(ctx.org.id, machine)) {
      ws.close(4404, 'not found');
      return;
    }
    sandbox = !machine.service_id;
  } catch {
    ws.close(1011, 'fleet unavailable');
    return;
  }

  if (closed) return;
  ready = true;
  for (const data of early) handle(data);
  early.length = 0;
}

function start(ws: TerminalSocket, machineId: string, sandbox: boolean, rows: number, cols: number) {
  const stream = fleet.machines.execStream(machineId, SHELL, {
    ...(sandbox ? { user: 'sprite' } : {}),
    tty: true,
    rows,
    cols,
  });

  // A PTY merges the two output streams, so everything arrives on stdout and
  // stderr never produces a byte. It is still drained, because an unread
  // stream on this SDK grows without limit.
  stream.stdout.on('data', (chunk: Buffer) => {
    ws.send(JSON.stringify({ type: 'data', data: chunk.toString('base64') }));
  });
  stream.stderr.resume();

  stream.on('error', (err: Error) => {
    ws.send(JSON.stringify({ type: 'error', message: err.message }));
  });

  void stream
    .wait()
    .then((code) => {
      ws.send(JSON.stringify({ type: 'exit', code }));
      ws.close(1000, 'shell exited');
    })
    .catch((err: Error) => {
      ws.send(JSON.stringify({ type: 'error', message: err.message }));
      ws.close(1011, 'stream failed');
    });

  ws.send(JSON.stringify({ type: 'session' }));
  return stream;
}

/**
 * Validates a client message. Anything else is dropped, never guessed at.
 *
 * The decode is `socketJson`, not a `typeof data === 'string'` branch: `ws`
 * delivers a frame as a Buffer, so treating the non-string case as an
 * already-parsed object gives an object with no `type` and drops every message
 * on the floor.
 */
function parse(data: unknown): ClientMessage | null {
  const parsed = socketJson<ClientMessage>(data);
  if (!parsed || typeof parsed.type !== 'string') return null;
  // A single frame is one keystroke or one paste. The ceiling is generous for
  // a paste and small enough that a socket cannot be used as a buffer.
  if (typeof parsed.data === 'string' && parsed.data.length > 1 << 20) return null;
  return parsed;
}
