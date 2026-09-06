/**
 * <command-palette>, in a real DOM.
 *
 * A browser test is REQUIRED. Everything here is a key the visitor presses on
 * `document`, and the served bytes are a closed trigger button whether the
 * shortcut works or not.
 *
 * The shortcut is asserted from both sides: that Ctrl K and Cmd K open it, and
 * that a bare `k` does not. A palette that opens on a plain letter makes every
 * text field in the app unusable.
 *
 * Counterfactual: drop the modifier check and the fourth test fails while the
 * second still passes, which is the shape that bug would have.
 */

const assert = {
  ok: (v, msg) => {
    if (!v) throw new Error(msg || 'Expected truthy');
  },
  equal: (a, b, msg) => {
    if (a !== b) throw new Error(msg || `Expected ${b}, got ${a}`);
  },
  includes: (haystack, needle, msg) => {
    if (!haystack.includes(needle)) throw new Error(msg || `Expected to find ${needle}`);
  },
};

async function mount() {
  await import('../../../components/command-palette.ts');
  const el = document.createElement('command-palette');
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

function press(key, opts = {}) {
  document.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true, ...opts }));
}

/** Wait until `check()` is true, or give up after two seconds. */
async function until(check) {
  for (let i = 0; i < 200 && !check(); i += 1) await new Promise((r) => setTimeout(r, 10));
  return check();
}

suite('command-palette', () => {
  teardown(() => {
    document.body.innerHTML = '';
  });

  test('it starts closed, showing the shortcut rather than hiding it', async () => {
    const el = await mount();
    assert.ok(el.querySelector('button[aria-label="Search this organisation"]'), 'a trigger is rendered');
    assert.includes(el.textContent, 'Ctrl');
    assert.equal(el.querySelector('[role="dialog"]'), null, 'and no dialog yet');
  });

  test('Ctrl K opens it and Escape closes it', async () => {
    const el = await mount();

    press('k', { ctrlKey: true });
    await el.updateComplete;
    assert.ok(el.querySelector('[role="dialog"]'), 'the dialog opened');
    assert.ok(el.querySelector('input[type="search"]'), 'with an input to type into');

    press('Escape');
    await el.updateComplete;
    assert.equal(el.querySelector('[role="dialog"]'), null, 'and Escape closes it');
  });

  test('Cmd K opens it too, because half the readers are on a Mac', async () => {
    const el = await mount();
    press('k', { metaKey: true });
    await el.updateComplete;
    assert.ok(el.querySelector('[role="dialog"]'));
  });

  test('a bare k is left alone, so typing a name is still typing', async () => {
    const el = await mount();
    press('k');
    await el.updateComplete;
    assert.equal(el.querySelector('[role="dialog"]'), null, 'no modifier, no palette');
  });

  test('the arrow keys move the selection and wrap at both ends', async () => {
    const el = await mount();
    press('k', { ctrlKey: true });
    await el.updateComplete;

    // The hits are seeded rather than searched for. This harness has no
    // session, so the real query correctly answers a signed-out caller with
    // nothing, and what is under test here is the keyboard, not the query.
    el.hits = [
      { kind: 'service', id: 'svc-web', label: 'web', href: '/services/svc-web' },
      { kind: 'machine', id: 'm-1', label: 'box', href: '/machines/m-1' },
      { kind: 'page', id: 'usage', label: 'Usage', href: '/usage' },
    ];
    await until(() => el.querySelectorAll('[role="option"]').length === 3);
    await el.updateComplete;

    const options = () => [...el.querySelectorAll('[role="option"]')];
    const selected = () => options().findIndex((o) => o.getAttribute('aria-selected') === 'true');

    assert.equal(selected(), 0, 'the first result starts selected');

    press('ArrowDown');
    await el.updateComplete;
    assert.equal(selected(), 1);

    // Up from the first wraps to the last, which is what every palette does
    // and what a modulo gives for free.
    press('ArrowUp');
    press('ArrowUp');
    await el.updateComplete;
    assert.equal(selected(), 2, 'wrapped to the end');
  });

  test('a signed-out caller is told nothing matched, not shown another org', async () => {
    // This harness has no session cookie, so the query returns the signed-out
    // marker. The palette must render that as an empty result rather than
    // treating a non-array as a list.
    const el = await mount();
    press('k', { ctrlKey: true });
    await until(() => el.textContent.includes('Nothing matches that.'));
    assert.includes(el.textContent, 'Nothing matches that.');
    assert.equal(el.querySelectorAll('[role="option"]').length, 0);
  });

  test('the dialog names itself for a screen reader', async () => {
    const el = await mount();
    press('k', { ctrlKey: true });
    await el.updateComplete;

    const dialog = el.querySelector('[role="dialog"]');
    assert.equal(dialog.getAttribute('aria-modal'), 'true');
    assert.ok(dialog.getAttribute('aria-label'));
    assert.ok(el.querySelector('[role="listbox"]').getAttribute('aria-label'), 'and so does the result list');
  });
});
