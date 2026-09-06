/**
 * <list-filter>, in a real DOM.
 *
 * A browser test is REQUIRED: this element hides rows in a table it does not
 * own, on a keystroke, and the served markup is identical whether it works.
 *
 * The `/` shortcut is the interesting half. Every tool this competes with
 * focuses its filter on `/`, and every one of them has to NOT do that while
 * the visitor is typing a name into a field. Both directions are asserted,
 * because getting the second one wrong makes every text input in the app drop
 * a character.
 *
 * Counterfactual: remove the editable-target guard and the last test fails
 * while everything else passes.
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
};

/** A table with three named rows, plus the filter pointed at it. */
async function mount() {
  await import('../../../components/list-filter.ts');
  const table = document.createElement('table');
  table.id = 'rows';
  table.innerHTML =
    '<tbody>' +
    ['alpha', 'beta', 'gamma'].map((name) => `<tr><td>${name}</td></tr>`).join('') +
    '</tbody>';
  const el = document.createElement('list-filter');
  el.setAttribute('for', 'rows');
  el.setAttribute('placeholder', 'Filter rows');
  document.body.append(el, table);
  await el.updateComplete;
  return { el, table };
}

function visible(table) {
  return [...table.querySelectorAll('tbody tr')].filter((tr) => !tr.hidden).map((tr) => tr.textContent);
}

function type(el, value) {
  const input = el.querySelector('input');
  input.value = value;
  input.dispatchEvent(new Event('input', { bubbles: true }));
  return input;
}

suite('list-filter', () => {
  teardown(() => {
    document.body.innerHTML = '';
  });

  test('typing hides the rows that do not match, and clearing brings them back', async () => {
    const { el, table } = await mount();
    assert.deepEqual(visible(table), ['alpha', 'beta', 'gamma']);

    type(el, 'a');
    assert.deepEqual(visible(table), ['alpha', 'beta', 'gamma'], 'every row contains an a');

    type(el, 'gam');
    assert.deepEqual(visible(table), ['gamma']);

    type(el, '');
    assert.deepEqual(visible(table), ['alpha', 'beta', 'gamma'], 'an empty filter hides nothing');
  });

  test('the match ignores case, because nobody types an id in the right case', async () => {
    const { el, table } = await mount();
    type(el, 'BETA');
    assert.deepEqual(visible(table), ['beta']);
  });

  test('it says how many rows survived, out loud', async () => {
    const { el } = await mount();
    type(el, 'gam');
    const status = el.querySelector('[role="status"]');
    assert.equal(status.getAttribute('aria-live'), 'polite');
    assert.equal(status.textContent, '1 shown');
  });

  test('slash focuses the filter', async () => {
    const { el } = await mount();
    const input = el.querySelector('input');
    input.blur();

    document.dispatchEvent(new KeyboardEvent('keydown', { key: '/', bubbles: true, cancelable: true }));
    assert.equal(document.activeElement, input, 'the filter took focus');
  });

  test('slash is left alone while the visitor is typing somewhere else', async () => {
    const { el } = await mount();
    const other = document.createElement('input');
    document.body.appendChild(other);
    other.focus();

    // A slash typed into a text field is a slash, not a shortcut. Stealing it
    // makes every input in the app drop a character.
    other.dispatchEvent(new KeyboardEvent('keydown', { key: '/', bubbles: true, cancelable: true }));
    assert.equal(document.activeElement, other, 'focus stayed where the visitor put it');
    assert.ok(el.querySelector('input') !== document.activeElement);
  });
});
