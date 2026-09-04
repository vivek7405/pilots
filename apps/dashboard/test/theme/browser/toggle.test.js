/**
 * The theme toggle, in a real DOM.
 *
 * A browser test is REQUIRED rather than nice to have. Everything this element
 * does happens after hydration and outside its own subtree: it writes
 * `localStorage`, sets `data-theme` on `<html>`, and toggles the `.dark` class
 * the kit's `dark:` variants key on. The SSR bytes are identical whether any of
 * that works, so no server test can tell a working toggle from a dead one.
 *
 * The two writes are not redundant. `data-theme` drives `color-scheme`, which
 * is what picks a side of every `light-dark()` token in the layout. The `.dark`
 * class drives the `dark:` variants compiled into the kit's helpers, which
 * cannot read `color-scheme`. Drop either one and half the page changes theme.
 *
 * Counterfactuals: stop writing `data-theme` and the token colours stay put;
 * stop toggling `.dark` and buttons, badges and inputs keep their light
 * variants on a dark page; drop the `localStorage` write and the choice is gone
 * on the next load.
 */

const assert = {
  equal: (a, b, msg) => {
    if (a !== b) throw new Error(msg || `Expected ${b}, got ${a}`);
  },
  ok: (v, msg) => {
    if (!v) throw new Error(msg || 'Expected truthy');
  },
};

const root = document.documentElement;

async function mount() {
  await import('../../../components/theme-toggle.ts');
  const el = document.createElement('theme-toggle');
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
}

/** Click the toggle's button and wait for the re-render it schedules. */
async function click(el) {
  el.querySelector('button').click();
  await el.updateComplete;
}

suite('theme-toggle', () => {
  setup(() => {
    localStorage.removeItem('pilots_theme');
    delete root.dataset.theme;
    root.classList.remove('dark');
  });

  teardown(() => {
    localStorage.removeItem('pilots_theme');
    delete root.dataset.theme;
    root.classList.remove('dark');
    document.body.innerHTML = '';
  });

  test('it starts on system, which forces no scheme of its own', async () => {
    const el = await mount();
    assert.equal(root.dataset.theme, undefined, 'nothing is forced, so the OS decides');
    el.remove();
  });

  test('it cycles system -> light -> dark -> system', async () => {
    const el = await mount();

    await click(el);
    assert.equal(root.dataset.theme, 'light');
    assert.equal(localStorage.getItem('pilots_theme'), 'light');

    await click(el);
    assert.equal(root.dataset.theme, 'dark');
    assert.equal(localStorage.getItem('pilots_theme'), 'dark');

    await click(el);
    assert.equal(root.dataset.theme, undefined, 'back to following the OS');
    assert.equal(localStorage.getItem('pilots_theme'), null, 'and the stored choice is cleared, not set to "system"');

    el.remove();
  });

  test('dark also sets the .dark class the kit variants key on', async () => {
    const el = await mount();
    await click(el); // light
    assert.equal(root.classList.contains('dark'), false, 'an explicit light beats a dark OS');
    await click(el); // dark
    assert.equal(root.classList.contains('dark'), true);
    el.remove();
  });

  test('it resumes the stored choice rather than starting over', async () => {
    localStorage.setItem('pilots_theme', 'dark');
    const el = await mount();

    // Reading the stored value is what keeps the button's label honest: the
    // pre-paint script has already painted dark, so a toggle that started at
    // "system" would need two clicks to reach light and show the wrong icon.
    await click(el);
    assert.equal(root.dataset.theme, undefined, 'the next step after dark is system');
    el.remove();
  });

  test('the button carries the current theme in its accessible name', async () => {
    const el = await mount();
    const label = () => el.querySelector('button').getAttribute('aria-label');
    assert.ok(/system/.test(label()), `an icon-only button needs a name, got: ${label()}`);
    await click(el);
    assert.ok(/light/.test(label()), `and the name follows the state, got: ${label()}`);
    el.remove();
  });
});
