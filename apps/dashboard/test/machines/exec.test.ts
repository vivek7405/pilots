/**
 * The exec console socket.
 *
 * `handle()` does not perform a WebSocket upgrade, so the `WS` export is
 * called directly with a fake socket. That is the honest boundary: what this
 * covers is the handler's own contract -- authenticate, check tenancy, forward
 * the frames, then the exit code, then close -- and the upgrade itself is the
 * framework's.
 *
 * Counterfactuals: drop the `requireOrg` call and the "signed out closes 4401"
 * assertion fails; pass `stdin: true` and the stdin assertion fails; send the
 * exit before draining stdout and the ordering assertion fails.
 */

import assert from 'node:assert/strict';
import { after, before, beforeEach, test } from 'node:test';
import { bootApp, signInAs, routeCtx, deferred } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';
import type { Machine } from '@pilots/sdk';

let app: TestApp;
let cookieA = '';
let orgA = '';
let WS: typeof import('#app/api/machines/[id]/exec/route.ts')['WS'];

interface Sent {
  type: string;
  data?: string;
  code?: number;
  message?: string;
}

/** A socket that records what the handler sent and how it closed. */
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
  return new Request('http://localhost/api/machines/m-alice/exec', {
    headers: cookie ? { cookie } : {},
  });
}

before(async () => {
  app = await bootApp();
  cookieA = await signInAs(app.handle, { id: 100, login: 'alice' });
  const { db } = await import('#db/connection.server.ts');
  orgA = (await db.query.orgs.findMany()).find((o) => o.slug === 'alice')!.id;
  app.fleet.data.machines.push({
    id: 'm-alice',
    name: 'm-alice',
    state: 'running',
    org_id: orgA,
    url: '',
    host_id: 'h1',
    created_at: 0,
  } as Machine);
  app.fleet.data.machines.push({
    id: 'm-bob',
    name: 'm-bob',
    state: 'running',
    org_id: 'someone-else',
    url: '',
    host_id: 'h1',
    created_at: 0,
  } as Machine);
  ({ WS } = await import('#app/api/machines/[id]/exec/route.ts'));
});

beforeEach(() => {
  app.fleet.data.execFrames.length = 0;
  app.fleet.data.lastExec = null;
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

test('a signed-out socket is closed 4401 and never reaches the fleet', async () => {
  const ws = fakeSocket();
  await WS(ws, request(), routeCtx({ id: 'm-alice' }));
  assert.deepEqual(ws.closed, { code: 4401, reason: 'unauthorized' });
  assert.equal(app.fleet.data.lastExec, null);
});

test("a foreign machine's console is closed 4404", async () => {
  const ws = fakeSocket();
  await WS(ws, request(cookieA), routeCtx({ id: 'm-bob' }));
  assert.equal(ws.closed?.code, 4404);
  assert.equal(app.fleet.data.lastExec, null);
});

test('output frames arrive before the exit code, then the socket closes', async () => {
  app.fleet.data.execFrames.push({ frame: 1, data: 'hi\n' }, { frame: 2, data: 'oops\n' }, { frame: 3, data: '3' });

  const ws = fakeSocket();
  await WS(ws, request(cookieA), routeCtx({ id: 'm-alice' }));
  ws.emit('message', JSON.stringify({ cmd: ['sh', '-c', 'echo hi'] }));
  await ws.whenClosed;

  assert.deepEqual(
    ws.sent.map((m) => m.type),
    ['stdout', 'stderr', 'exit'],
    'the exit is last, so a viewer never sees a code before the output that produced it',
  );
  assert.equal(Buffer.from(ws.sent[0].data!, 'base64').toString(), 'hi\n');
  assert.equal(Buffer.from(ws.sent[1].data!, 'base64').toString(), 'oops\n');
  assert.equal(ws.sent[2].code, 3);
  assert.equal(ws.closed?.code, 1000);
});

test('the command runs as `sprite` with stdin off', async () => {
  app.fleet.data.execFrames.push({ frame: 3, data: '0' });
  const ws = fakeSocket();
  await WS(ws, request(cookieA), routeCtx({ id: 'm-alice' }));
  ws.emit('message', JSON.stringify({ cmd: ['ls'], dir: '/home/sprite/app' }));
  await ws.whenClosed;

  const exec = app.fleet.data.lastExec!;
  assert.deepEqual(exec.argv, ['ls']);
  assert.equal(exec.opts.user, 'sprite');
  assert.equal(exec.opts.stdin, false, 'a process holding an open stdin it never reads would hang');
  assert.equal(exec.opts.cwd, '/home/sprite/app');
});

test('a malformed command is refused rather than guessed at', async () => {
  const ws = fakeSocket();
  await WS(ws, request(cookieA), routeCtx({ id: 'm-alice' }));
  ws.emit('message', JSON.stringify({ cmd: 'rm -rf /' }));

  assert.equal(ws.sent[0].type, 'error');
  assert.equal(app.fleet.data.lastExec, null, 'nothing ran');
  assert.equal(ws.closed, null, 'the socket stays open so the viewer can retry');
});
