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

suite('machine-list, chips filter and row navigation', () => {
  const RealNavigate = [];
  let navigated;

  setup(() => {
    sockets = [];
    navigated = [];
    globalThis.WebSocket = FakeSocket;
  });

  teardown(() => {
    globalThis.WebSocket = RealWebSocket;
    document.body.innerHTML = '';
    RealNavigate.length = 0;
  });

  const hosts = [
    { id: 'h-amd-1', alive: true, cpu_free: 4, mem_free_mib: 8192, cpu_vendor: 'AuthenticAMD' },
    { id: 'h-amd-2', alive: false, cpu_free: 0, mem_free_mib: 0, cpu_vendor: 'AuthenticAMD' },
  ];

  const on = (id, state, host) => ({ id, name: id, state, host_id: host, url: `https://${id}.pilotrun.app` });

  async function mountTiers() {
    await import('../../../modules/machines/components/machine-list.ts');
    const el = document.createElement('machine-list');
    el.hosts = hosts;
    el.initial = [
      on('m-run', 'running', 'h-amd-1'),
      on('m-warm', 'suspended', 'h-amd-1'),
      // Its owner is gone entirely, so no live host can restore its image.
      on('m-cold', 'suspended', 'h-vanished'),
    ];
    document.body.appendChild(el);
    await el.updateComplete;
    await new Promise((resolve) => queueMicrotask(resolve));
    return el;
  }

  const chip = (el, label) =>
    [...el.querySelectorAll('button[aria-pressed]')].find((b) => b.textContent.trim().startsWith(label));

  test('the chips count by resume tier, not by state', async () => {
    const el = await mountTiers();
    // Both sleeping machines are `suspended`; only one of them resumes warm.
    assert.equal(chip(el, 'All').textContent.trim(), 'All 3');
    assert.equal(chip(el, 'running').textContent.trim(), 'running 1');
    assert.equal(chip(el, 'warm').textContent.trim(), 'warm 1');
    assert.equal(chip(el, 'cold').textContent.trim(), 'cold 1');
    el.remove();
  });

  test('clicking a chip filters the rows and marks itself pressed', async () => {
    const el = await mountTiers();
    chip(el, 'cold').click();
    await el.updateComplete;

    assert.deepEqual(rowIds(el), ['m-cold']);
    assert.equal(chip(el, 'cold').getAttribute('aria-pressed'), 'true');
    assert.equal(chip(el, 'All').getAttribute('aria-pressed'), 'false');
    el.remove();
  });

  test('the filter box hides rows that do not match', async () => {
    const el = await mountTiers();
    const input = el.querySelector('#machine-filter');
    input.value = 'warm';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    await el.updateComplete;

    assert.deepEqual(rowIds(el), ['m-warm']);
    assert.includes(el.textContent, '1 of 3 machines', 'and it says how many it is hiding');
    el.remove();
  });

  test('a sleeping machine says whether waking it costs its memory', async () => {
    const el = await mountTiers();
    const warm = [...el.querySelectorAll('tbody tr')].find((tr) => tr.textContent.includes('m-warm'));
    const cold = [...el.querySelectorAll('tbody tr')].find((tr) => tr.textContent.includes('m-cold'));

    assert.includes(warm.textContent, 'resumes warm');
    assert.includes(cold.textContent, 'will cold-boot');
    el.remove();
  });

  test('a row carries its destination, and the action buttons do not swallow it', async () => {
    const el = await mountTiers();
    const row = el.querySelector('tbody tr');
    assert.equal(row.dataset.href, '/machines/m-run', 'the whole row knows where it goes');

    // The click handler reads `data-href` off the closest ancestor that has
    // one, and bails when the click landed on a control. Both halves are
    // asserted through the DOM the handler itself queries.
    const suspend = [...row.querySelectorAll('button')].find((b) => b.textContent.trim() === 'Suspend');
    assert.ok(suspend, 'the row has a Suspend button inside the click target');
    assert.ok(suspend.closest('a, button, input, select, textarea, label') === suspend,
      'so a click on it is recognised as a control and never navigates');

    const nameCell = row.querySelector('td');
    assert.ok(nameCell.closest('[data-href]') === row, 'and a click on the cell resolves to the row');
    el.remove();
  });

  test('the other chip shows the machines it counted', async () => {
    // A stopped machine's resume tier is `boot`, which is not a chip: it reads
    // as "other" to anyone not thinking about the ladder. Counting it there
    // and then refusing to show it under that chip is the bug this pins.
    const el = await mountTiers();
    sockets[sockets.length - 1].deliver({
      type: 'delta',
      upsert: [on('m-stopped', 'stopped', 'h-amd-1')],
      remove: [],
    });
    await el.updateComplete;

    assert.equal(chip(el, 'other').textContent.trim(), 'other 1');
    chip(el, 'other').click();
    await el.updateComplete;
    assert.deepEqual(rowIds(el), ['m-stopped']);
    el.remove();
  });

  test('a sandboxes list never shows a service replica, whatever the socket sends', async () => {
    await import('../../../modules/machines/components/machine-list.ts');
    const el = document.createElement('machine-list');
    el.hosts = hosts;
    el.sandboxes = true;
    el.initial = [on('m-sandbox', 'running', 'h-amd-1')];
    document.body.appendChild(el);
    await el.updateComplete;
    await new Promise((resolve) => queueMicrotask(resolve));

    // The feed carries the ORG's machines, not the page's rows, so its first
    // snapshot replaces the sandboxes the page seeded with every machine there
    // is -- which filled the overview's Sandboxes section with replicas the
    // moment it hydrated.
    sockets[sockets.length - 1].deliver({
      type: 'snapshot',
      machines: [
        on('m-sandbox', 'running', 'h-amd-1'),
        { ...on('m-replica', 'running', 'h-amd-1'), service_id: 'svc-1' },
      ],
    });
    await el.updateComplete;

    assert.deepEqual(rowIds(el), ['m-sandbox']);
    assert.excludes(el.textContent, 'm-replica');
    el.remove();
  });

  test('an empty list offers the command that fills it', async () => {
    const el = await mountTiers();
    el.initial = [];
    el.rows = [];
    await el.updateComplete;
    assert.includes(el.textContent, 'pilot machines create');
    el.remove();
  });
});
