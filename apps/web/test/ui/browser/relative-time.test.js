/**
 * `<relative-time>` after hydration, and the `duration` form in particular.
 *
 * A browser test is REQUIRED rather than nice to have. The server renders the
 * ABSOLUTE time inside the element, so the SSR bytes are identical whether the
 * relative text works, whether it says "ago", and whether the `duration`
 * attribute reaches the component as a prop at all. A boolean prop that never
 * arrives is invisible to every server test: the element would simply keep
 * saying "8 hours ago" inside a sentence that already said "since".
 *
 * Counterfactual: drop `duration: prop(Boolean)` from the component and the
 * second test here fails while every server test stays green.
 */

const assert = {
  equal: (a, b, msg) => {
    if (a !== b) throw new Error(msg || `Expected ${b}, got ${a}`);
  },
  ok: (v, msg) => {
    if (!v) throw new Error(msg || 'Expected truthy');
  },
};

/** Mount one, `hours` in the past, and return its rendered text. */
async function mount(hours, attrs = {}) {
  await import('../../../components/relative-time.ts');
  const el = document.createElement('relative-time');
  // Seconds since the epoch, which is what the engine stamps and what the
  // attribute therefore carries: a STRING of digits.
  el.setAttribute('datetime', String(Math.floor((Date.now() - hours * 3_600_000) / 1000)));
  for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, v);
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

suite('relative-time', () => {
  teardown(() => {
    document.body.innerHTML = '';
  });

  test('the default form says the direction', async () => {
    const el = await mount(8);
    assert.equal(el.textContent.trim(), '8 hours ago');
  });

  test('the duration form says the magnitude and never the direction', async () => {
    const el = await mount(8, { duration: '' });
    assert.equal(el.textContent.trim(), '8 hours');
    assert.ok(!el.textContent.includes('ago'), 'a sentence saying "since" must not get "ago" too');
  });

  test('both forms keep the exact moment in the title and the datetime', async () => {
    // The browser only ever replaces the TEXT. Losing the machine-readable
    // value to a rounded phrase would make the exact time unrecoverable.
    const el = await mount(8, { duration: '' });
    const time = el.querySelector('time');
    assert.ok(time.getAttribute('datetime').startsWith('20'), 'an ISO datetime survives');
    assert.ok(time.getAttribute('title').includes('UTC'), 'the absolute time is the title');
  });
});
