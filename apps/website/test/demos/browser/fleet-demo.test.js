/**
 * Browser tests for <fleet-demo>.
 *
 * The demo's whole claim is that recovery needs no coordination: every
 * survivor runs the same placement function and the slices tile. So the tests
 * assert the PROPERTIES that makes true (no machine lost, none on a dead host,
 * placement stable across recomputation) rather than pinning which host each
 * machine lands on. Pinning the exact assignment would make the test a
 * restatement of the hash function, and it would break the moment a host name
 * changed for reasons that have nothing to do with correctness.
 */
import '#components/fleet-demo.ts';

const assert = {
  ok: (v, msg) => { if (!v) throw new Error(msg || `Expected truthy, got ${v}`); },
  equal: (a, b, msg) => { if (a !== b) throw new Error(msg || `Expected ${JSON.stringify(b)}, got ${JSON.stringify(a)}`); },
};

const mount = async () => {
  const el = document.createElement('fleet-demo');
  document.body.appendChild(el);
  await el.updateComplete;
  return el;
};

/** Every host card, in DOM order. */
const cards = (el) => [...el.querySelectorAll('ul')].map((ul) => ul.closest('div'));
const machinesOn = (card) =>
  [...card.querySelectorAll('li')].map((li) => li.textContent.trim().replace(/ rescued$/, ''))
    .filter((t) => t !== 'no machines');
const killButtons = (el) =>
  [...el.querySelectorAll('button')].filter((b) => b.textContent.trim() === 'kill -9');

const allMachines = (el) => cards(el).flatMap(machinesOn).sort();

suite('fleet-demo', () => {
  test('starts with every host healthy and every machine placed', async () => {
    const el = await mount();
    assert.equal(cards(el).length, 3, 'three hosts');
    assert.equal(allMachines(el).length, 6, 'six machines');
    assert.equal(killButtons(el).length, 3, 'every host can be killed');
    el.remove();
  });

  test('killing a host loses no machines', async () => {
    const el = await mount();
    const before = allMachines(el);
    killButtons(el)[0].click();
    await el.updateComplete;
    const after = allMachines(el);
    assert.equal(after.length, before.length, 'the same number of machines exist');
    assert.equal(after.join(','), before.join(','), 'and they are the same machines');
    el.remove();
  });

  test('a dead host holds nothing', async () => {
    const el = await mount();
    killButtons(el)[0].click();
    await el.updateComplete;
    const dead = cards(el)[0];
    assert.equal(machinesOn(dead).length, 0, 'the killed host has no machines left on it');
    el.remove();
  });

  test('rescued machines are marked as rescued', async () => {
    const el = await mount();
    killButtons(el)[0].click();
    await el.updateComplete;
    const rescued = [...el.querySelectorAll('li')].filter((li) => /rescued/.test(li.textContent));
    assert.equal(rescued.length, 2, 'both of the dead host’s machines are marked');
    el.remove();
  });

  test('placement is deterministic: kill, revive, kill again lands identically', async () => {
    const el = await mount();
    killButtons(el)[0].click();
    await el.updateComplete;
    const first = cards(el).map(machinesOn).map((m) => m.join('|')).join(' / ');

    // revive
    [...el.querySelectorAll('button')].find((b) => b.textContent.trim() === 'revive').click();
    await el.updateComplete;
    killButtons(el)[0].click();
    await el.updateComplete;
    const second = cards(el).map(machinesOn).map((m) => m.join('|')).join(' / ');

    assert.equal(second, first, 'the same rule over the same inputs gives the same placement');
    el.remove();
  });

  test('the last surviving host cannot be killed', async () => {
    const el = await mount();
    killButtons(el)[0].click();
    await el.updateComplete;
    killButtons(el)[0].click();
    await el.updateComplete;
    const remaining = killButtons(el);
    assert.ok(remaining.every((b) => b.disabled), 'a fleet of zero has nowhere to rescue to');
    assert.equal(allMachines(el).length, 6, 'and all six machines are still placed');
    el.remove();
  });
});
