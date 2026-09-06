/**
 * <app-nav>, in a real DOM.
 *
 * A browser test is REQUIRED rather than nice to have. The root layout is
 * PRESERVED across a client-router navigation, so a highlight the server
 * computed is correct in the served bytes and then freezes: every soft
 * navigation after the first leaves the wrong link lit. The SSR markup is
 * identical whether this element re-derives the active link or not, so only a
 * real navigation event can tell the two apart.
 *
 * Counterfactual: remove the `webjs:navigate` listener and the second and
 * third assertions here fail while the first still passes, which is exactly
 * the shape the bug had.
 *
 * The `popstate` half of the wiring is deliberately not asserted here. The real
 * client router is live in this page, and it handles `popstate` by fetching the
 * new URL and swapping the document, which disconnects the element under test
 * before any assertion can read it. Dispatching a synthetic `popstate` tests
 * the router, not this component. `webjs:navigate` is the router's own signal
 * and is safe to raise, so that is the path asserted.
 */

const assert = {
  equal: (a, b, msg) => {
    if (a !== b) throw new Error(msg || `Expected ${b}, got ${a}`);
  },
  ok: (v, msg) => {
    if (!v) throw new Error(msg || 'Expected truthy');
  },
};

async function mount(current) {
  await import('../../../components/app-nav.ts');
  const el = document.createElement('app-nav');
  el.setAttribute('current', current);
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

/** The href of the link the nav says is the current page. */
function lit(el) {
  return el.querySelector('[aria-current="page"]')?.getAttribute('href') ?? null;
}

/** What the client router does on a soft navigation, minus the content swap. */
async function softNavigate(el, path) {
  history.pushState({}, '', path);
  document.dispatchEvent(new CustomEvent('webjs:navigate'));
  await el.updateComplete;
}

suite('app-nav', () => {
  const startPath = location.pathname;

  teardown(() => {
    history.replaceState({}, '', startPath);
    document.body.innerHTML = '';
  });

  test('the current attribute seeds the first paint', async () => {
    history.replaceState({}, '', '/services');
    const el = await mount('/services');
    assert.equal(lit(el), '/services');
  });

  test('a soft navigation moves the highlight', async () => {
    history.replaceState({}, '', '/services');
    const el = await mount('/services');
    await softNavigate(el, '/machines');
    assert.equal(lit(el), '/machines', 'the highlight followed the router, not the server render');
  });

  test('a section owns its subroutes, and Overview does not own everything', async () => {
    history.replaceState({}, '', '/services');
    const el = await mount('/services');

    await softNavigate(el, '/services/svc-1');
    assert.equal(lit(el), '/services', 'a detail page keeps its section lit');

    await softNavigate(el, '/machines/m-1');
    assert.equal(lit(el), '/machines');

    // '/' is matched exactly, or it would be lit on every page in the app.
    await softNavigate(el, '/');
    assert.equal(lit(el), '/');
  });

  test('the nav names itself for a screen reader', async () => {
    const el = await mount('/');
    assert.equal(el.querySelector('nav').getAttribute('aria-label'), 'Primary');
  });
});
