/**
 * The live feed, and the reason it does not use `broadcast`.
 *
 * `broadcast(path, data)` sends to EVERY socket on that path. One
 * `broadcast('/api/machines', ...)` would therefore hand org A's machines to
 * every viewer of org B, which is a cross-tenant leak with no error and no log
 * line. So this module keeps a per-org subscriber map, and the test that
 * matters most is the one that watches two orgs at once.
 *
 * Counterfactual: replace the per-org send in `tick()` with one `broadcast`
 * and "org B never receives org A's rows" fails.
 */

import assert from 'node:assert/strict';
import { after, before, beforeEach, test } from 'node:test';
import { bootApp } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';
import type { Machine } from '@pilots/sdk';

let app: TestApp;
let live: typeof import('#modules/machines/live.server.ts');

function machine(id: string, orgId: string, state = 'running'): Machine {
  return { id, name: id, state, org_id: orgId, url: '', host_id: 'h1', created_at: 0 } as Machine;
}

function socket() {
  const messages: { type: string; machines?: Machine[]; upsert?: Machine[]; remove?: string[] }[] = [];
  return {
    messages,
    send(data: string) {
      messages.push(JSON.parse(data));
    },
    close() {},
  };
}

before(async () => {
  app = await bootApp();
  live = await import('#modules/machines/live.server.ts');
});

beforeEach(() => {
  app.fleet.data.machines.length = 0;
});

after(() => {
  delete (globalThis as { __pilots_fleet?: unknown }).__pilots_fleet;
});

test('a subscriber gets an opening snapshot of its own org only', async () => {
  app.fleet.data.machines.push(machine('a1', 'org-a'), machine('b1', 'org-b'));

  const ws = socket();
  await live.subscribe('org-a', ws);
  try {
    assert.equal(ws.messages.length, 1);
    assert.equal(ws.messages[0].type, 'snapshot');
    assert.deepEqual(ws.messages[0].machines!.map((m) => m.id), ['a1']);
  } finally {
    live.unsubscribe('org-a', ws);
  }
});

test('org B never receives org A rows, on the snapshot or on a delta', async () => {
  app.fleet.data.machines.push(machine('a1', 'org-a'), machine('b1', 'org-b'));

  const a = socket();
  const b = socket();
  await live.subscribe('org-a', a);
  await live.subscribe('org-b', b);

  try {
    // Change ONLY org A's machine, then tick.
    app.fleet.data.machines.find((m) => m.id === 'a1')!.state = 'suspended';
    await live.tick();

    const aDelta = a.messages.filter((m) => m.type === 'delta');
    assert.equal(aDelta.length, 1, 'org A saw its own change');
    assert.deepEqual(aDelta[0].upsert!.map((m) => m.id), ['a1']);

    assert.equal(
      b.messages.filter((m) => m.type === 'delta').length,
      0,
      'org B got no message at all, let alone one carrying a1',
    );
    const everySeen = b.messages.flatMap((m) => [...(m.machines ?? []), ...(m.upsert ?? [])]);
    assert.equal(everySeen.some((m) => m.org_id === 'org-a'), false);
  } finally {
    live.unsubscribe('org-a', a);
    live.unsubscribe('org-b', b);
  }
});

test('a delta carries removals and skips rows nothing visible changed on', async () => {
  app.fleet.data.machines.push(machine('a1', 'org-a'), machine('a2', 'org-a'));

  const ws = socket();
  await live.subscribe('org-a', ws);
  try {
    // Nothing changed: no delta at all, so an idle fleet sends no traffic.
    await live.tick();
    assert.equal(ws.messages.filter((m) => m.type === 'delta').length, 0);

    app.fleet.data.machines.splice(1, 1);
    await live.tick();

    const delta = ws.messages.filter((m) => m.type === 'delta').at(-1)!;
    assert.deepEqual(delta.remove, ['a2']);
    assert.deepEqual(delta.upsert, []);
  } finally {
    live.unsubscribe('org-a', ws);
  }
});

test('a second viewer joining before the tick does not swallow a pending delta', async () => {
  app.fleet.data.machines.push(machine('a1', 'org-a'));

  const first = socket();
  await live.subscribe('org-a', first);
  const second = socket();
  try {
    // The change lands, and a second tab opens BEFORE the next tick.
    app.fleet.data.machines[0].state = 'suspended';
    await live.subscribe('org-a', second);
    assert.equal(second.messages[0].machines![0].state, 'suspended', 'the newcomer starts from the current state');

    await live.tick();

    const delta = first.messages.filter((m) => m.type === 'delta');
    assert.equal(delta.length, 1, 'the first tab still receives the change it has not seen');
    assert.equal(delta[0].upsert![0].id, 'a1');
    assert.equal(delta[0].upsert![0].state, 'suspended');
  } finally {
    live.unsubscribe('org-a', first);
    live.unsubscribe('org-a', second);
  }
});

test('a lone viewer whose snapshot already carries the state gets no delta for it', async () => {
  // The counterpart: seeding on the FIRST subscribe is still right, so an
  // unchanged fleet stays silent rather than replaying the snapshot as a delta.
  app.fleet.data.machines.push(machine('a1', 'org-a', 'suspended'));

  const ws = socket();
  await live.subscribe('org-a', ws);
  try {
    await live.tick();
    assert.equal(ws.messages.filter((m) => m.type === 'delta').length, 0);
  } finally {
    live.unsubscribe('org-a', ws);
  }
});

test('the shared tick stops when the last subscriber leaves', async () => {
  const ws = socket();
  await live.subscribe('org-a', ws);
  assert.equal(live.subscriberCount('org-a'), 1);

  live.unsubscribe('org-a', ws);
  assert.equal(live.subscriberCount('org-a'), 0);

  app.fleet.calls.length = 0;
  await live.tick();
  assert.equal(app.fleet.calls.length, 0, 'an unwatched fleet is never listed');
});
