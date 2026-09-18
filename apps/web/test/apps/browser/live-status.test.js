/**
 * `<live-status>` replacing a server-rendered line, in a real DOM.
 *
 * A browser test is REQUIRED for the part that matters: the element renders
 * `<slot>` until the feed speaks, so the ONLY way to tell "went live" from
 * "still showing what the server sent" is to mount it, deliver a message, and
 * read what the DOM says afterwards. A server test sees the slot and stops.
 *
 * `deliver()` stands in for the socket. It drives the same `apply` the socket's
 * `onMessage` drives, so the delta rules under test are the shipped ones and
 * not a second implementation written for the test.
 *
 * Counterfactual: drop the `live` guard so `render()` always computes, and the
 * first assert fails -- the element would replace the server's line with a
 * count derived from no rows at all, which is the hydration flash this design
 * exists to avoid.
 */

const assert = {
  ok: (v, msg) => {
    if (!v) throw new Error(msg || 'expected truthy');
  },
  match: (text, re, msg) => {
    if (!re.test(text)) throw new Error(`${msg || 'no match'}: ${re} against ${JSON.stringify(text)}`);
  },
  noMatch: (text, re, msg) => {
    if (re.test(text)) throw new Error(`${msg || 'unexpected match'}: ${re} against ${JSON.stringify(text)}`);
  },
};

const SERVICE = { id: 'svc-1', name: 'web', app: 'shop', replicas: 1, release_id: 'rel-2' };

function machine(over) {
  return { id: 'm-1', name: 'web-1', service_id: 'svc-1', release_id: 'rel-2', state: 'suspended', ...over };
}

async function mount(kind, services, slotHtml) {
  await import('../../../modules/apps/components/live-status.ts');
  const el = document.createElement('live-status');
  el.setAttribute('kind', kind);
  el.services = services;
  el.releases = {};
  el.innerHTML = slotHtml;
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

async function settle(el) {
  await el.updateComplete;
  await new Promise((r) => setTimeout(r, 0));
  await el.updateComplete;
}

suite('live-status', () => {
  teardown(() => {
    document.body.innerHTML = '';
  });

  test('the server line stands until the feed speaks', async () => {
    const el = await mount('app', [SERVICE], '<span>SERVER LINE</span>');
    assert.match(el.textContent, /SERVER LINE/, 'the slot content is what shows before any message');
  });

  test('a woken machine flips the line with no reload', async () => {
    const { deliver } = await import('../../../modules/machines/live-client.ts');
    const el = await mount('app', [SERVICE], '<span>SERVER LINE</span>');

    // Asleep: still online, because a sleeping replica answers the next request.
    deliver({ type: 'snapshot', machines: [machine({ state: 'suspended' })] });
    await settle(el);
    assert.noMatch(el.textContent, /SERVER LINE/, 'the element took over from the server markup');
    assert.match(el.textContent, /1\/1 service online/, `went live: ${el.textContent}`);

    // The engine gives up on it: the same element says so without a reload.
    deliver({ type: 'delta', upsert: [machine({ state: 'error' })], remove: [] });
    await settle(el);
    assert.match(el.textContent, /0\/1 service online/, `a failed replica drops the count: ${el.textContent}`);
  });

  test('one socket serves every element on the page', async () => {
    const { listenerCount, deliver } = await import('../../../modules/machines/live-client.ts');
    const a = await mount('app', [SERVICE], '<span>A</span>');
    const b = await mount('card', [SERVICE], '<span>B</span>');
    assert.ok(listenerCount() >= 2, `both elements share the feed, saw ${listenerCount()}`);

    deliver({ type: 'snapshot', machines: [machine({ state: 'running' })] });
    await settle(a);
    await settle(b);
    assert.match(a.textContent, /1\/1 service online/, `app line: ${a.textContent}`);
    // The card speaks the product's vocabulary, not the engine's: a running
    // machine reads `Online`, which is the word the whole dashboard uses.
    assert.match(b.textContent, /Online/, `card line went live too: ${b.textContent}`);

    const before = listenerCount();
    a.remove();
    b.remove();
    await new Promise((r) => setTimeout(r, 0));
    assert.ok(listenerCount() < before, 'removing the elements gives their share of the socket back');
  });
});

/**
 * The drawer.
 *
 * The card that opens the panel went live first, and the panel behind it did
 * not, so opening a drawer on a machine that had just woken showed a `Sleeping`
 * badge beside a card that already said Online. Both halves of the drawer are
 * covered: the badge over the service, and the per-instance row.
 *
 * Counterfactual: return the slot for the empty-pills case instead of an empty
 * template, and the woken service below keeps its `Sleeping` badge forever --
 * the server's markup would be projected straight back.
 */
suite('live-status in the drawer', () => {
  teardown(() => {
    document.body.innerHTML = '';
  });

  const SLEEPY = { id: 'svc-1', name: 'web', app: 'shop', replicas: 1, release_id: 'rel-2' };

  test('the health badge clears when the service is no longer asleep', async () => {
    const { deliver } = await import('../../../modules/machines/live-client.ts');
    const el = await mount('pills', [SLEEPY], '<span class="badge">Sleeping</span>');
    el.releases = { 'svc-1': [{ id: 'rel-2', healthy: true, created_at: 1 }] };

    deliver({ type: 'snapshot', machines: [machine({ state: 'suspended' })] });
    await settle(el);
    assert.match(el.textContent, /Sleeping/, 'still asleep, so the badge stands');

    deliver({ type: 'delta', upsert: [machine({ state: 'running' })], remove: [] });
    await settle(el);
    assert.noMatch(el.textContent, /Sleeping/, `the badge goes when the machine wakes: ${el.textContent}`);
  });

  test('an instance row follows its own machine', async () => {
    const { deliver } = await import('../../../modules/machines/live-client.ts');
    await import('../../../modules/machines/components/live-machine-state.ts');
    const el = document.createElement('live-machine-state');
    el.setAttribute('machine-id', 'm-1');
    el.setAttribute('mode', 'since');
    el.innerHTML = '<span>SERVER ROW</span>';
    document.body.appendChild(el);
    await el.updateComplete;
    assert.match(el.textContent, /SERVER ROW/, 'the server markup shows until the feed speaks');

    deliver({ type: 'snapshot', machines: [machine({ state: 'running' })] });
    await settle(el);
    assert.match(el.textContent, /Online/, `the row went live: ${el.textContent}`);

    deliver({ type: 'delta', upsert: [machine({ state: 'suspended' })], remove: [] });
    await settle(el);
    assert.match(el.textContent, /Sleeping/, `and follows it back down: ${el.textContent}`);
  });

  test('a row whose machine the feed never mentions keeps the server markup', async () => {
    const { deliver } = await import('../../../modules/machines/live-client.ts');
    await import('../../../modules/machines/components/live-machine-state.ts');
    const el = document.createElement('live-machine-state');
    el.setAttribute('machine-id', 'm-absent');
    el.innerHTML = '<span>SERVER ROW</span>';
    document.body.appendChild(el);
    await el.updateComplete;

    deliver({ type: 'snapshot', machines: [machine({ state: 'running' })] });
    await settle(el);
    assert.match(el.textContent, /SERVER ROW/, 'an unmentioned machine is not erased, only left stale');
  });
});
