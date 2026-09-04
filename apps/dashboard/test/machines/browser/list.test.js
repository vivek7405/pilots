/**
 * The live machine list, in a real DOM.
 *
 * A browser test is REQUIRED here rather than nice to have: applying a delta to
 * the rendered rows is post-hydration behaviour, so the SSR bytes are identical
 * whether it works or not and a server test cannot see the difference. A
 * wrongly applied delta shows up as a stale row and nothing else.
 *
 * The seam is `globalThis.WebSocket`, not the framework's `connectWS`. Stubbing
 * the constructor means the component's REAL subscription code runs -- the same
 * `connectWS` production uses, with its own JSON decoding and its own
 * open/close wiring -- against messages this test chooses.
 *
 * Counterfactual: make the delta handler ignore `remove` and the row-count
 * assertion fails; mutate `this.rows` in place instead of replacing the array
 * and no re-render happens at all.
 */

const assert = {
  ok: (v, msg) => {
    if (!v) throw new Error(msg || 'Expected truthy');
  },
  equal: (a, b, msg) => {
    if (a !== b) throw new Error(msg || `Expected ${b}, got ${a}`);
  },
  deepEqual: (a, b, msg) => {
    if (JSON.stringify(a) !== JSON.stringify(b)) {
      throw new Error(`${msg || 'Not equal'}: ${JSON.stringify(a)} vs ${JSON.stringify(b)}`);
    }
  },
  includes: (haystack, needle, msg) => {
    if (!haystack.includes(needle)) throw new Error(msg || `Expected to find ${needle}`);
  },
  excludes: (haystack, needle, msg) => {
    if (haystack.includes(needle)) throw new Error(msg || `Expected NOT to find ${needle}`);
  },
};

/** The sockets `connectWS` opened during a test, newest last. */
let sockets = [];
const RealWebSocket = globalThis.WebSocket;

class FakeSocket {
  static OPEN = 1;
  constructor(url) {
    this.url = url;
    this.readyState = 1;
    this.sent = [];
    this.closed = false;
    sockets.push(this);
    // `connectWS` assigns its handlers synchronously right after construction,
    // so opening on a microtask is what a real socket's timing looks like here.
    queueMicrotask(() => this.onopen?.({}));
  }
  send(data) {
    this.sent.push(data);
  }
  close() {
    this.closed = true;
    // A deliberate close must NOT look like a drop, or connectWS reconnects.
    this.onclose = null;
  }
  deliver(message) {
    this.onmessage?.({ data: JSON.stringify(message) });
  }
}

function machine(id, state) {
  return { id, name: id, state, host_id: 'h1', url: `https://${id}.pilotrun.app` };
}

async function mount(initial) {
  await import('../../../modules/machines/components/machine-list.ts');
  const el = document.createElement('machine-list');
  el.initial = initial;
  document.body.appendChild(el);
  await el.updateComplete;
  // Let the queued open fire so the component is in its connected state.
  await new Promise((resolve) => queueMicrotask(resolve));
  return el;
}

function rowIds(el) {
  return [...el.querySelectorAll('tbody tr')].map((tr) => tr.querySelector('a').textContent.trim());
}

suite('machine-list', () => {
  setup(() => {
    sockets = [];
    globalThis.WebSocket = FakeSocket;
  });

  teardown(() => {
    globalThis.WebSocket = RealWebSocket;
    document.body.innerHTML = '';
  });

  test('renders the rows the server already sent, before any socket traffic', async () => {
    const el = await mount([machine('m1', 'running'), machine('m2', 'suspended')]);
    assert.deepEqual(rowIds(el), ['m1', 'm2']);
    assert.includes(el.textContent, 'suspended');
    el.remove();
  });

  test('it subscribes to the per-org machines path', async () => {
    const el = await mount([]);
    assert.equal(sockets.length, 1, 'exactly one socket');
    assert.includes(String(sockets[0].url), '/api/machines');
    el.remove();
  });

  test('a delta updates a state in place and removes a row', async () => {
    const el = await mount([machine('m1', 'running'), machine('m2', 'running')]);

    sockets[0].deliver({ type: 'delta', upsert: [machine('m1', 'suspended')], remove: ['m2'] });
    await el.updateComplete;

    assert.deepEqual(rowIds(el), ['m1'], 'the removed machine is gone');
    assert.includes(el.textContent, 'suspended', 'and the surviving row shows its new state');
    assert.excludes(el.textContent, 'm2');
    el.remove();
  });

  test('a delta can add a machine that was not in the snapshot', async () => {
    const el = await mount([machine('m1', 'running')]);

    sockets[0].deliver({ type: 'delta', upsert: [machine('m3', 'starting')], remove: [] });
    await el.updateComplete;

    assert.deepEqual(rowIds(el), ['m1', 'm3']);
    el.remove();
  });

  test('a snapshot replaces the whole list', async () => {
    const el = await mount([machine('m1', 'running'), machine('m2', 'running')]);

    sockets[0].deliver({ type: 'snapshot', machines: [machine('m9', 'running')] });
    await el.updateComplete;

    assert.deepEqual(rowIds(el), ['m9']);
    el.remove();
  });

  test('the socket is closed when the element leaves the document', async () => {
    const el = await mount([machine('m1', 'running')]);
    el.remove();
    await new Promise((resolve) => setTimeout(resolve, 0));
    assert.equal(sockets[0].closed, true, 'a navigation away takes the subscription with it');
  });

  test('an empty list says so rather than rendering an empty table', async () => {
    const el = await mount([]);
    assert.includes(el.textContent, 'No machines yet');
    assert.equal(el.querySelectorAll('tbody tr').length, 0);
    el.remove();
  });
});
