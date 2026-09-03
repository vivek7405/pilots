/**
 * Browser tests for <lifecycle-demo>.
 *
 * The component exists to make one argument: a machine's URL survives every
 * lifecycle event. A demo that silently stopped transitioning would still LOOK
 * fine in a screenshot while making that argument dishonestly, so the address
 * assertion below is the one that matters most. It is checked after every
 * transition rather than once at the end, because "the URL never changed" is
 * the claim, not "the URL ended up the same".
 */
import '#components/lifecycle-demo.ts';

const assert = {
  ok: (v, msg) => { if (!v) throw new Error(msg || `Expected truthy, got ${v}`); },
  equal: (a, b, msg) => { if (a !== b) throw new Error(msg || `Expected ${JSON.stringify(b)}, got ${JSON.stringify(a)}`); },
};

const mount = async () => {
  const el = document.createElement('lifecycle-demo');
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
};

/** The button whose label matches, ignoring disabled ones. */
const btn = (el, label) =>
  [...el.querySelectorAll('button')].find((b) => b.textContent.trim() === label);

const url = (el) => el.textContent.match(/bold-otter\.[a-z.]+/)?.[0];
const field = (el, name) => {
  const dt = [...el.querySelectorAll('dt')].find((d) => d.textContent.trim() === name);
  return dt && dt.nextElementSibling.textContent.trim();
};

suite('lifecycle-demo', () => {
  test('starts running, as a sandbox, with nothing survived yet', async () => {
    const el = await mount();
    assert.equal(field(el, 'state'), 'running');
    assert.equal(field(el, 'autoStop'), 'suspend', 'a sandbox suspends when idle');
    assert.equal(field(el, 'minRunning'), '0', 'a sandbox may scale to zero');
    assert.ok(url(el), 'the machine URL is rendered');
    el.remove();
  });

  test('the URL is identical after every transition', async () => {
    const el = await mount();
    const original = url(el);
    assert.ok(original, 'there is a URL to compare against');

    for (const label of ['suspend', 'wake', 'checkpoint', 'restore', 'promote']) {
      const b = btn(el, label);
      assert.ok(b, `the ${label} button exists`);
      assert.ok(!b.disabled, `${label} is available at this point in the lifecycle`);
      b.click();
      await el.updateComplete;
      assert.equal(url(el), original, `the URL is unchanged after ${label}`);
    }
    el.remove();
  });

  test('promote flips the knobs that define a production service', async () => {
    const el = await mount();
    btn(el, 'promote').click();
    await el.updateComplete;
    assert.equal(field(el, 'autoStop'), 'off', 'a service does not suspend itself');
    assert.equal(field(el, 'minRunning'), '1', 'a service keeps one running');
    assert.ok(!btn(el, 'promote') || btn(el, 'promote').disabled, 'promote is spent once used');
    el.remove();
  });

  test('suspend and wake are mutually exclusive', async () => {
    const el = await mount();
    assert.ok(btn(el, 'wake').disabled, 'a running machine cannot be woken');
    btn(el, 'suspend').click();
    await el.updateComplete;
    assert.equal(field(el, 'state'), 'suspended');
    assert.ok(btn(el, 'suspend').disabled, 'a suspended machine cannot be suspended again');
    assert.ok(!btn(el, 'wake').disabled, 'and can now be woken');
    el.remove();
  });

  test('restore is unavailable until a checkpoint exists', async () => {
    const el = await mount();
    assert.ok(btn(el, 'restore').disabled, 'nothing to restore to yet');
    btn(el, 'checkpoint').click();
    await el.updateComplete;
    assert.equal(field(el, 'checkpoints'), '1');
    assert.ok(!btn(el, 'restore').disabled, 'a checkpoint makes restore available');
    el.remove();
  });

  test('reset returns it to the starting state', async () => {
    const el = await mount();
    btn(el, 'promote').click();
    await el.updateComplete;
    [...el.querySelectorAll('button')].find((b) => b.textContent.trim() === 'reset').click();
    await el.updateComplete;
    assert.equal(field(el, 'autoStop'), 'suspend', 'back to sandbox knobs');
    assert.equal(field(el, 'checkpoints'), '0');
    el.remove();
  });
});
