/**
 * The terminal socket.
 *
 * `handle()` does not perform a WebSocket upgrade, so the `WS` export is
 * called directly with a fake socket. That is the honest boundary: what this
 * covers is the handler's contract -- authenticate, check tenancy, open the
 * stream with the window the client asked for, and relay both directions --
 * and the upgrade itself is the framework's.
 *
 * The interesting assertion is the third one. A shell reads its window size at
 * startup, so the stream must not be opened until the client has said how big
 * its terminal is; opening at 24 by 80 and resizing a frame later makes the
 * first prompt redraw where the reader can see it.
 *
 * Counterfactuals: drop the `requireOrg` call and the 4401 assertion fails;
 * drop `tty: true` and the PTY assertion fails; open the stream before the
 * `open` message arrives and the window assertion reports 24 by 80.
 */

import assert from 'node:assert/strict';
import { after, before, beforeEach, test } from 'node:test';
import { bootApp, signInAs, routeCtx, deferred } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';
import type { Machine } from '@pilots/sdk';

let app: TestApp;
let cookieA = '';
let orgA = '';
let WS: (typeof import('#app/api/machines/[id]/terminal/route.ts'))['WS'];

interface Sent {
  type: string;
  data?: string;
  code?: number;
  message?: string;
}

function fakeSocket() {
  const sent: Sent[] = [];
  const handlers: Record<string, ((data?: unknown) => void)[]> = {};
  let closed: { code?: number; reason?: string } | null = null;
  const closedPromise = deferred<{ code?: number; reason?: string }>();
  return {
    sent,
    get closed() {
      return closed;
    },
    whenClosed: closedPromise.promise,
    send(data: string) {
      sent.push(JSON.parse(data) as Sent);
    },
    close(code?: number, reason?: string) {
      if (closed) return;
      closed = { code, reason };
      closedPromise.resolve(closed);
    },
    on(event: string, fn: (data?: unknown) => void) {
      (handlers[event] ??= []).push(fn);
    },
    emit(event: string, data?: unknown) {
      for (const fn of handlers[event] ?? []) fn(data);
    },
  };
}

function request(cookie?: string): Request {
  return new Request('http://localhost/api/machines/m-1/terminal', {
    headers: cookie ? { cookie } : {},
  });
}

before(async () => {
  app = await bootApp();
  cookieA = await signInAs(app.handle, { id: 7400, login: 'terminaller' });
  const { db } = await import('#db/connection.server.ts');
  orgA = (await db.query.orgs.findMany()).find((o) => o.slug === 'terminaller')!.id;
  ({ WS } = await import('#app/api/machines/[id]/terminal/route.ts'));
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

beforeEach(() => {
  app.fleet.reset();
  app.fleet.data.execHold = true;
  app.fleet.data.machines.push({ id: 'm-1', name: 'box', state: 'running', org_id: orgA } as unknown as Machine);
  app.fleet.data.machines.push({ id: 'm-r', name: 'web-1', state: 'running', org_id: orgA, service_id: 'svc-w' } as unknown as Machine);
});

test('a signed-out socket is closed 4401 and never reaches the fleet', async () => {
  const ws = fakeSocket();
  await WS(ws, request(), routeCtx({ id: 'm-1' }));

  assert.deepEqual(ws.closed, { code: 4401, reason: 'unauthorized' });
  assert.equal(app.fleet.calls.filter((c) => c.method === 'machines.execStream').length, 0);
});

test("another org's machine is 4404, the same answer as one that never existed", async () => {
  const cookieB = await signInAs(app.handle, { id: 7401, login: 'outsider' });
  const ws = fakeSocket();
  await WS(ws, request(cookieB), routeCtx({ id: 'm-1' }));

  assert.deepEqual(ws.closed, { code: 4404, reason: 'not found' });
  assert.equal(app.fleet.calls.filter((c) => c.method === 'machines.execStream').length, 0);
});

test('nothing opens until the client says how big its window is', async () => {
  const ws = fakeSocket();
  await WS(ws, request(cookieA), routeCtx({ id: 'm-1' }));

  // Authenticated, owned, and still no stream: the shell would otherwise
  // start at 24 by 80 and redraw its first prompt when the resize landed.
  assert.equal(app.fleet.data.lastExec, null);

  ws.emit('message', JSON.stringify({ type: 'open', rows: 40, cols: 120 }));
  const opened = app.fleet.data.lastExec!;
  assert.equal(opened.id, 'm-1');
  assert.deepEqual(opened.opts, { tty: true, rows: 40, cols: 120 });
});

test('an `open` sent before the handler has authenticated still starts the shell', async () => {
  const ws = fakeSocket();

  // NOT awaited. Every other test here awaits `WS` and only then emits, which
  // is what hid this for a whole session: the browser sends `open` from its own
  // `open` event, the instant the upgrade completes, while the handler is still
  // reading the session and asking hostd about the machine. `ws` buffers
  // nothing, so a listener attached after those awaits never sees the frame.
  const handled = WS(ws, request(cookieA), routeCtx({ id: 'm-1' }));
  ws.emit('message', JSON.stringify({ type: 'open', rows: 40, cols: 120 }));
  await handled;

  // Counterfactual: move `ws.on('message', ...)` back below the `requireOrg`
  // and `machines.get` awaits and `lastExec` is null here, which is the
  // "Connected, blinking cursor, no output" the user reported.
  const opened = app.fleet.data.lastExec;
  assert.ok(opened, 'the early frame was queued, not dropped');
  assert.deepEqual(opened.opts, { tty: true, rows: 40, cols: 120 });
});

test('an early frame on a socket that fails auth opens nothing', async () => {
  const ws = fakeSocket();
  const handled = WS(ws, request(), routeCtx({ id: 'm-1' }));
  ws.emit('message', JSON.stringify({ type: 'open', rows: 40, cols: 120 }));
  await handled;

  // Queueing must not become a way past the gate: the queue is drained only
  // after the session and the tenancy check have both passed.
  assert.deepEqual(ws.closed, { code: 4401, reason: 'unauthorized' });
  assert.equal(app.fleet.data.lastExec, null);
});

test('no terminal names a user, whatever kind of machine it is', async () => {
  // Sandbox and service replica alike. Naming one here would mean guessing
  // which generation of image is on the other end: the current rootfs has
  // `pilot`, one built before the rename has `sprite`, and an image from
  // someone's Dockerfile has neither. Only the guest agent can resolve that,
  // and it does, so this handler stays out of it.
  for (const id of ['m-1', 'm-r']) {
    const ws = fakeSocket();
    await WS(ws, request(cookieA), routeCtx({ id }));
    ws.emit('message', JSON.stringify({ type: 'open', rows: 24, cols: 80 }));
    const opened = app.fleet.data.lastExec!;
    assert.equal(opened.id, id);
    assert.deepEqual(opened.opts, { tty: true, rows: 24, cols: 80 }, `${id} named a user`);
  }
});

test('the shell is chosen in the guest, so an image without bash still gets one', async () => {
  const ws = fakeSocket();
  await WS(ws, request(cookieA), routeCtx({ id: 'm-1' }));
  ws.emit('message', JSON.stringify({ type: 'open', rows: 24, cols: 80 }));

  // Not `bash -l` from here: a built image may be alpine or distroless, and
  // asking for a shell that is not there ends the session with a start failure
  // instead of a prompt.
  //
  // And an ABSOLUTE path, because the guest agent execs this argv without a
  // PATH search: a bare `sh` starts nothing on a built image, and the terminal
  // then shows a cursor that swallows every keystroke. Verified against a live
  // replica: `sh -c 'echo OK'` closed with "start failed" while
  // `/bin/sh -c 'echo OK'` returned OK on the same machine.
  const argv = app.fleet.data.lastExec!.argv;
  assert.equal(argv[0], '/bin/sh', 'absolute: the agent does not search PATH');
  assert.match(argv[2]!, /command -v bash/);
  assert.match(argv[2]!, /exec \/bin\/sh -l -i/);
  // `-i` on both arms. Without it busybox ash -- `/bin/sh` on the alpine base
  // most built images use -- starts without printing a prompt, and the
  // terminal on a service replica is an empty rectangle that only answers if
  // you type into it blind.
  assert.match(argv[2]!, /exec bash -l -i/);
});

test('a second open is ignored rather than obeyed', async () => {
  const ws = fakeSocket();
  await WS(ws, request(cookieA), routeCtx({ id: 'm-1' }));

  ws.emit('message', JSON.stringify({ type: 'open', rows: 40, cols: 120 }));
  ws.emit('message', JSON.stringify({ type: 'open', rows: 10, cols: 10 }));

  const opens = app.fleet.calls.filter((c) => c.method === 'machines.execStream');
  assert.equal(opens.length, 1, 'one session per socket');
});

test('keystrokes go to the shell as bytes, and a resize reaches it as a resize', async () => {
  const ws = fakeSocket();
  await WS(ws, request(cookieA), routeCtx({ id: 'm-1' }));
  ws.emit('message', JSON.stringify({ type: 'open', rows: 24, cols: 80 }));

  ws.emit('message', JSON.stringify({ type: 'data', data: Buffer.from('ls\n').toString('base64') }));
  ws.emit('message', JSON.stringify({ type: 'resize', cols: 100, rows: 30 }));

  assert.equal(Buffer.concat(app.fleet.data.execStdin).toString(), 'ls\n');
  assert.deepEqual(app.fleet.data.execResizes, [{ cols: 100, rows: 30 }]);
});

test('a window size outside 1..65535 falls back rather than reaching the guest', async () => {
  const ws = fakeSocket();
  await WS(ws, request(cookieA), routeCtx({ id: 'm-1' }));
  // The guest closes the socket with 1008 on a bad size, so a client that
  // computed one wrongly would lose the session before it started.
  ws.emit('message', JSON.stringify({ type: 'open', rows: 0, cols: 999_999 }));

  assert.deepEqual(app.fleet.data.lastExec!.opts, { tty: true, rows: 24, cols: 80 });
});

test('output comes back base64, and a message before open is dropped', async () => {
  app.fleet.data.execHold = false;
  app.fleet.data.execFrames.push({ frame: 1, data: 'hello' }, { frame: 3, data: '0' });

  const ws = fakeSocket();
  await WS(ws, request(cookieA), routeCtx({ id: 'm-1' }));

  // Sent before `open`: there is no stream to write to, and inventing one
  // would open a session the client never sized.
  ws.emit('message', JSON.stringify({ type: 'data', data: 'aGk=' }));
  assert.equal(app.fleet.data.lastExec, null);
  assert.equal(app.fleet.data.execStdin.length, 0);

  ws.emit('message', JSON.stringify({ type: 'open', rows: 24, cols: 80 }));
  await ws.whenClosed;

  assert.equal(ws.sent[0]!.type, 'session');
  const data = ws.sent.find((m) => m.type === 'data');
  assert.equal(Buffer.from(data!.data!, 'base64').toString(), 'hello');
  assert.equal(ws.sent.at(-1)!.type, 'exit');
});

test('a frame arrives as a Buffer, which is what the socket library delivers', async () => {
  const ws = fakeSocket();
  await WS(ws, request(cookieA), routeCtx({ id: 'm-1' }));

  // Not a string. `ws` hands the handler raw bytes, so a route that branches
  // on `typeof data === 'string'` and casts the rest straight to its message
  // interface gets an object with no `type` and drops everything. The exec
  // console shipped with exactly that bug and its Run button did nothing.
  ws.emit('message', Buffer.from(JSON.stringify({ type: 'open', rows: 30, cols: 100 })));

  assert.ok(app.fleet.data.lastExec, 'the open message was understood');
  assert.deepEqual(app.fleet.data.lastExec!.opts, { tty: true, rows: 30, cols: 100 });
});

test('closing the socket kills the shell, so a closed tab leaves nothing running', async () => {
  const ws = fakeSocket();
  await WS(ws, request(cookieA), routeCtx({ id: 'm-1' }));
  ws.emit('message', JSON.stringify({ type: 'open', rows: 24, cols: 80 }));

  ws.emit('close');
  // `kill` settles the held stream, which is what the fake's hold mode makes
  // observable: without the close handler this promise never resolves and a
  // machine with a live shell never goes idle.
  await ws.whenClosed;
});
