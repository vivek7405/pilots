/**
 * `<log-stream>`'s live counter, in a real DOM.
 *
 * A browser test is REQUIRED. The counter is moved by `fetch` outcomes inside
 * a private async method; the server renders no part of it, so no server test
 * can tell a correct count from a wrong one.
 *
 * The case that matters is a MIXED set: one source that streams and one that
 * does not. The increment happens only after the response is known good, but
 * the decrement in `finally` used to run either way, so a bad source took a
 * healthy stream off the badge. It read "1 live" with two streams running.
 *
 * Counterfactual: drop the `counted` guard and decrement unconditionally, and
 * the mixed case below reports one fewer than is running.
 */

const assert = {
  equal: (a, b, msg) => {
    if (a !== b) throw new Error(msg || `Expected ${b}, got ${a}`);
  },
};

const realFetch = globalThis.fetch;

/** A response whose body never ends, so the stream stays open for the assert. */
function openStream() {
  return new Response(new ReadableStream({ start() {} }), { status: 200 });
}

/** Route each machine id to a canned outcome. */
function stubFetch(byId) {
  globalThis.fetch = (url) => {
    const id = String(url).match(/\/api\/machines\/([^/]+)\/logs/)?.[1];
    const outcome = byId[id];
    if (outcome === 'missing') return Promise.resolve(new Response('no such machine', { status: 404 }));
    if (outcome === 'throws') return Promise.reject(new Error('network down'));
    return Promise.resolve(openStream());
  };
}

async function mount(sources) {
  await import('../../../modules/logs/components/log-stream.ts');
  const el = document.createElement('log-stream');
  el.sources = sources;
  document.body.appendChild(el);
  await el.updateComplete;
  // The opens are async: let their first awaits settle before asserting.
  await new Promise((r) => setTimeout(r, 0));
  await el.updateComplete;
  return el;
}

suite('log-stream live count', () => {
  teardown(() => {
    globalThis.fetch = realFetch;
    document.body.innerHTML = '';
  });

  test('every healthy source counts once', async () => {
    stubFetch({ 'm-1': 'ok', 'm-2': 'ok' });
    const el = await mount([
      { id: 'm-1', service: 'web', instance: 'web-1' },
      { id: 'm-2', service: 'web', instance: 'web-2' },
    ]);
    assert.equal(el.live, 2);
  });

  test('a source that 404s does not take a live stream off the count', async () => {
    stubFetch({ 'm-1': 'ok', 'm-2': 'missing', 'm-3': 'ok' });
    const el = await mount([
      { id: 'm-1', service: 'web', instance: 'web-1' },
      { id: 'm-2', service: 'web', instance: 'gone' },
      { id: 'm-3', service: 'web', instance: 'web-3' },
    ]);
    assert.equal(el.live, 2, 'two streams are open, so the badge says two');
  });

  test('a fetch that throws before it counted does not decrement either', async () => {
    stubFetch({ 'm-1': 'ok', 'm-2': 'throws' });
    const el = await mount([
      { id: 'm-1', service: 'web', instance: 'web-1' },
      { id: 'm-2', service: 'web', instance: 'unreachable' },
    ]);
    assert.equal(el.live, 1);
  });

  test('every source failing leaves the count at zero, not below it', async () => {
    stubFetch({ 'm-1': 'missing', 'm-2': 'throws' });
    const el = await mount([
      { id: 'm-1', service: 'web', instance: 'gone' },
      { id: 'm-2', service: 'web', instance: 'unreachable' },
    ]);
    assert.equal(el.live, 0);
  });
});
