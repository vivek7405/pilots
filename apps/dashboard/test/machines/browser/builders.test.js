/**
 * Builders in the live list, in a real DOM.
 *
 * A browser test rather than a server one, for the reason the rest of this
 * component's suite is: which rows a chip shows is decided AFTER hydration,
 * on every socket delta, so the SSR bytes look the same whether the filter
 * works or not. A builder leaking into the default list shows up as one extra
 * row and nothing else.
 *
 * Counterfactuals, each turning one assertion below red:
 *
 *   - drop the `!isBuilder` filter in `base()` and the builder appears under
 *     All, so a team of one service looks like a team of two
 *   - count `builders` off `base()` instead of the whole pool and the chip
 *     reads 0 until it is clicked, which is the one moment it is not needed
 *   - keep the URL cell for a builder and the list offers a link to an address
 *     that answers nothing
 *   - drop the badge and a revealed builder is indistinguishable from a
 *     sandbox somebody made
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
    queueMicrotask(() => this.onopen?.({}));
  }
  send(data) {
    this.sent.push(data);
  }
  close() {
    this.closed = true;
    this.onclose = null;
  }
  deliver(message) {
    this.onmessage?.({ data: JSON.stringify(message) });
  }
}

const hosts = [{ id: 'h1', alive: true, cpu_free: 4, mem_free_mib: 8192, cpu_vendor: 'AuthenticAMD' }];

const row = (id, name, state) => ({
  id,
  name,
  state,
  host_id: 'h1',
  url: `https://${id}.pilotrun.app`,
});

/** What hostd names a builder: the prefix four things in the engine read. */
const builder = (id, host) => row(id, `builder-org1-${host}`, 'suspended');

async function mount(initial) {
  await import('../../../modules/machines/components/machine-list.ts');
  const el = document.createElement('machine-list');
  el.hosts = hosts;
  el.initial = initial;
  document.body.appendChild(el);
  await el.updateComplete;
  await new Promise((resolve) => queueMicrotask(resolve));
  return el;
}

const rowIds = (el) => [...el.querySelectorAll('tbody tr')].map((tr) => tr.querySelector('a').textContent.trim());

const chip = (el, label) =>
  [...el.querySelectorAll('button[aria-pressed]')].find((b) => b.textContent.trim().startsWith(label));

suite('machine-list, builders', () => {
  setup(() => {
    sockets = [];
    globalThis.WebSocket = FakeSocket;
  });

  teardown(() => {
    globalThis.WebSocket = RealWebSocket;
    document.body.innerHTML = '';
  });

  test('a builder is out of the default list and out of every count but its own', async () => {
    const el = await mount([row('m-app', 'm-app', 'running'), builder('m-b', 'h1')]);

    assert.deepEqual(rowIds(el), ['m-app'], 'nobody created the builder, so it is not in the list of what they made');
    assert.equal(chip(el, 'All').textContent.trim(), 'All 1');
    assert.equal(chip(el, 'Builders').textContent.trim(), 'Builders 1', 'the count is what keeps it from being invisible');
    el.remove();
  });

  test('the builders chip reveals them, with a badge and no URL', async () => {
    const el = await mount([row('m-app', 'm-app', 'running'), builder('m-b', 'h1')]);

    chip(el, 'Builders').click();
    await el.updateComplete;

    assert.deepEqual(rowIds(el), ['builder-org1-h1'], 'and only them');
    assert.equal(chip(el, 'Builders').getAttribute('aria-pressed'), 'true');

    const tr = el.querySelector('tbody tr');
    assert.includes(tr.textContent, 'builder', 'the row says what it is rather than looking like a sandbox');
    assert.excludes(tr.innerHTML, 'pilotrun.app', 'a builder answers no request, so it is offered no link');
    el.remove();
  });

  test('a builder arriving on a delta stays out of the list it was not in', async () => {
    const el = await mount([row('m-app', 'm-app', 'running')]);

    sockets[sockets.length - 1].deliver({ type: 'delta', upsert: [builder('m-late', 'h9')], remove: [] });
    await el.updateComplete;

    assert.deepEqual(rowIds(el), ['m-app'], 'the filter runs on every delta, not only on the seeded rows');
    assert.equal(chip(el, 'Builders').textContent.trim(), 'Builders 1');
    el.remove();
  });

  test('a team whose only row is a builder still gets the empty state, and can still reach it', async () => {
    const el = await mount([builder('m-only', 'h1')]);

    assert.includes(el.textContent, 'Nothing here yet', 'a builder is not something the team deployed');
    assert.ok(chip(el, 'Builders'), 'but the chip is still there, or the row would be unreachable rather than hidden');

    chip(el, 'Builders').click();
    await el.updateComplete;
    assert.deepEqual(rowIds(el), ['builder-org1-h1']);
    el.remove();
  });
});
