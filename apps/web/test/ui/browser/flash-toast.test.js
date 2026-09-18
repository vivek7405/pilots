/**
 * <flash-toast>, in a real DOM.
 *
 * A browser test is REQUIRED. This element does all of its work in
 * `connectedCallback`, which SSR never calls, so the served bytes are the same
 * empty custom element whether it works or not.
 *
 * What it has to get right is a pair: publish exactly one toast for the
 * outcome the redirect carried, and then STRIP the parameter, or a reload or a
 * shared link repeats a message about something that already happened.
 *
 * Counterfactual: drop the `history.replaceState` and the last assertion here
 * fails while the toast still appears, which is the version that looks fine
 * until someone refreshes.
 */

const assert = {
  equal: (a, b, msg) => {
    if (a !== b) throw new Error(msg || `Expected ${b}, got ${a}`);
  },
  ok: (v, msg) => {
    if (!v) throw new Error(msg || 'Expected truthy');
  },
};

let toasts = [];

async function mountViewport() {
  const sonner = await import('../../../components/ui/sonner.ts');
  // Record what is published rather than waiting on the viewport's animation:
  // what is under test is the mapping and the URL cleanup, not the kit's toast.
  const bus = (globalThis.__webjsSonnerBus ??= { add() {}, remove() {} });
  bus.add = (item) => toasts.push(item);
  return sonner;
}

async function mountAt(search) {
  history.replaceState({}, '', '/machines' + search);
  await import('../../../components/flash-toast.ts');
  const el = document.createElement('flash-toast');
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

suite('flash-toast', () => {
  const startPath = location.pathname + location.search;

  setup(async () => {
    toasts = [];
    await mountViewport();
  });

  teardown(() => {
    history.replaceState({}, '', startPath);
    document.body.innerHTML = '';
  });

  test('an ok key publishes one success toast and leaves the URL clean', async () => {
    await mountAt('?ok=deployed');
    assert.equal(toasts.length, 1, 'exactly one toast');
    assert.equal(toasts[0].type, 'success');
    assert.equal(toasts[0].message, 'Deploy started.');
    assert.equal(location.search, '', 'the parameter is stripped so a reload does not repeat it');
  });

  test('an err key publishes an error toast', async () => {
    await mountAt('?err=deploy');
    assert.equal(toasts.length, 1);
    assert.equal(toasts[0].type, 'error');
    assert.equal(toasts[0].message, 'The deploy did not start.');
  });

  test('a key nobody defined never becomes the message', async () => {
    // The keys are a closed set because a message interpolated from the URL is
    // a message an attacker writes into a surface the visitor trusts.
    await mountAt('?err=%3Cscript%3Ealert(1)%3C%2Fscript%3E');
    assert.equal(toasts.length, 1);
    assert.equal(toasts[0].message, 'That did not work.');

    toasts = [];
    await mountAt('?ok=not-a-real-outcome');
    assert.equal(toasts.length, 0, 'an unknown ok key says nothing at all');
  });

  test('no parameter publishes nothing and touches no history entry', async () => {
    await mountAt('?page=2');
    assert.equal(toasts.length, 0);
    assert.equal(location.search, '?page=2', 'an unrelated query survives');
  });
});
