/**
 * <app-canvas> and <slide-over>, in a real DOM.
 *
 * Both are islands over server-drawn markup, so the served bytes are the
 * same whether they work. What only a browser can show: the stage scales to
 * fit a narrow viewport, the arrow keys move focus between cards, Escape on
 * the panel goes back to the canvas, and axe is clean on both in both
 * themes.
 *
 * The cards are rendered from the REAL fragment rather than transcribed, so
 * a change to the card's markup is under test here too.
 *
 * Counterfactuals: drop the ResizeObserver and the scale test fails; drop
 * the keydown listener and ArrowRight leaves focus where it was; drop the
 * Escape handler and no close intent is dispatched.
 */

import { assertNoA11yViolations } from '@webjsdev/core/testing';
import { render } from '@webjsdev/core';

const assert = {
  ok: (v, msg) => {
    if (!v) throw new Error(msg || 'Expected truthy');
  },
  equal: (a, b, msg) => {
    if (a !== b) throw new Error(msg || `Expected ${b}, got ${a}`);
  },
};

const root = document.documentElement;

function setTheme(theme) {
  root.dataset.theme = theme;
  root.classList.toggle('dark', theme === 'dark');
}

let stylesLoaded = false;
async function loadStyles() {
  if (stylesLoaded) return;
  const css = await fetch('/public/tailwind.css');
  assert.ok(css.ok, `the compiled stylesheet is served (run npm run css:build): ${css.status}`);
  const sheet = document.createElement('style');
  sheet.textContent = await css.text();
  document.head.appendChild(sheet);
  // The token block lives in the layout; borrow the login page's copy so the
  // colours axe measures are the app's own.
  const page = await fetch('/login');
  const doc = new DOMParser().parseFromString(await page.text(), 'text/html');
  for (const style of doc.querySelectorAll('style')) document.head.appendChild(style.cloneNode(true));
  document.body.style.background = 'var(--background)';
  document.body.style.color = 'var(--foreground)';
  stylesLoaded = true;
}

/** A three-service canvas: db on top, api under it, web under api, and one edge each. */
async function mountCanvas(width = 1000) {
  const [{ layoutApp }, { edgesSvg }, { serviceCard }] = await Promise.all([
    import('../../../modules/apps/utils/layout.ts'),
    import('../../../modules/apps/utils/ui/canvas-svg.ts'),
    import('../../../modules/apps/utils/ui/service-card.ts'),
    import('../../../modules/apps/components/app-canvas.ts'),
  ]);
  const { html } = await import('@webjsdev/core');
  const layout = layoutApp([
    { id: 'svc-web', name: 'web', dependsOn: ['api'] },
    { id: 'svc-api', name: 'api', dependsOn: ['db'] },
    { id: 'svc-db', name: 'db', dependsOn: [] },
    { id: 'svc-cache', name: 'cache', dependsOn: [] },
  ]);
  const host = document.createElement('div');
  host.style.width = `${width}px`;
  document.body.appendChild(host);
  render(
    html`<app-canvas class="block">
      <div
        data-canvas-stage
        data-width=${String(layout.width)}
        data-height=${String(layout.height)}
        class="relative grid gap-4 sm:block sm:h-[var(--stage-h)] sm:w-[var(--stage-w)]"
        style=${`--stage-w:${layout.width}px;--stage-h:${layout.height}px`}
      >
        <div class="pointer-events-none absolute inset-0 hidden sm:block" aria-hidden="true">${edgesSvg(layout)}</div>
        ${layout.placed.map((placed) =>
          serviceCard({
            app: 'gallery',
            service: { id: placed.id, name: placed.name, url: `https://${placed.name}.example` },
            placed,
            replicas: [{ id: `m-${placed.name}`, state: placed.name === 'cache' ? 'suspended' : 'running' }],
            volume: placed.name === 'db' ? { id: 'vol', name: 'db-data', size_gib: 20 } : undefined,
            selected: placed.name === 'web',
          }),
        )}
      </div>
    </app-canvas>`,
    host,
  );
  const el = host.querySelector('app-canvas');
  await el.updateComplete;
  // The ResizeObserver reports after layout and the fit lands a frame later.
  const stage = el.querySelector('[data-canvas-stage]');
  for (let i = 0; i < 20 && !stage.style.transform; i += 1) {
    await new Promise((resolve) => requestAnimationFrame(resolve));
  }
  return { el, host, layout };
}

suite('app-canvas', () => {
  suiteSetup(loadStyles);

  teardown(() => {
    delete root.dataset.theme;
    root.classList.remove('dark');
    for (const el of document.querySelectorAll('body > div')) el.remove();
  });

  test('the stage scales down to fit a narrow host and the host shrinks with it', async () => {
    const { el, layout } = await mountCanvas(400);
    const stage = el.querySelector('[data-canvas-stage]');
    const expected = 400 / layout.width;
    assert.ok(layout.width > 400, `the layout is wider than the host: ${layout.width}`);
    const match = /scale\(([\d.]+)\)/.exec(stage.style.transform);
    assert.ok(match, `a scale was applied: ${stage.style.transform}`);
    assert.ok(Math.abs(Number(match[1]) - expected) < 0.01, `scale ${match[1]} fits ${expected}`);
    assert.equal(stage.style.transformOrigin, 'left top', 'the browser serialises top left as left top');
    assert.equal(el.style.height, `${Math.ceil(layout.height * expected)}px`, 'the host takes the scaled height');
  });

  test('a host wider than the stage does not scale it up', async () => {
    const { el } = await mountCanvas(1400);
    const stage = el.querySelector('[data-canvas-stage]');
    assert.equal(stage.style.transform, 'scale(1)');
  });

  test('the arrow keys move focus to the nearest card in that direction', async () => {
    const { el } = await mountCanvas(1000);
    const card = (name) => el.querySelector(`[data-canvas-card][href$="svc-${name}"]`);
    // Top row by name: cache at x=0, db at x=288. api is alone on the second
    // row, so it takes the first column, under cache; web is alone on the third.
    card('cache').focus();
    assert.equal(document.activeElement, card('cache'));

    card('cache').dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true, cancelable: true }));
    assert.equal(document.activeElement, card('db'), 'right from cache is db');

    card('db').dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowDown', bubbles: true, cancelable: true }));
    assert.equal(document.activeElement, card('api'), 'down from db is api, the nearest card below');

    card('api').dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowUp', bubbles: true, cancelable: true }));
    assert.equal(document.activeElement, card('cache'), 'up from api is cache, straight above it');

    card('cache').focus();
    card('cache').dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowLeft', bubbles: true, cancelable: true }));
    assert.equal(document.activeElement, card('cache'), 'nothing to the left, so focus stays');
  });

  for (const theme of ['light', 'dark']) {
    test(`the canvas has no axe violations in ${theme}`, async () => {
      setTheme(theme);
      const { el } = await mountCanvas(1000);
      assert.equal(el.querySelectorAll('[data-canvas-card]').length, 4, 'all four cards render');
      await assertNoA11yViolations(el);
    });
  }
});

suite('slide-over', () => {
  suiteSetup(loadStyles);

  teardown(() => {
    delete root.dataset.theme;
    root.classList.remove('dark');
    for (const el of document.querySelectorAll('body > div, body > slide-over')) el.remove();
  });

  async function mountPanel() {
    await import('../../../components/slide-over.ts');
    const el = document.createElement('slide-over');
    el.setAttribute('back', '/apps/gallery');
    el.innerHTML =
      '<div class="p-6"><h2 id="service-panel-title" tabindex="-1" class="m-0 text-title font-semibold">web</h2>' +
      '<p><input aria-label="A field"></p></div>';
    document.body.appendChild(el);
    await el.updateComplete;
    await new Promise((resolve) => queueMicrotask(resolve));
    return el;
  }

  test('opening moves focus to the heading', async () => {
    const el = await mountPanel();
    assert.equal(document.activeElement, el.querySelector('h2'));
    assert.equal(el.querySelector('[role="dialog"]').getAttribute('aria-modal'), 'false', 'the canvas stays live behind it');
  });

  test('Escape asks to go back to the canvas', async () => {
    const el = await mountPanel();
    let intent = null;
    // Cancelling the intent keeps the real router from leaving the test page.
    el.addEventListener('slide-over-close', (e) => {
      intent = e.detail.back;
      e.preventDefault();
    });
    document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }));
    assert.equal(intent, '/apps/gallery');
  });

  test('Escape inside a text field is left to the field', async () => {
    const el = await mountPanel();
    let fired = false;
    el.addEventListener('slide-over-close', (e) => {
      fired = true;
      e.preventDefault();
    });
    const input = el.querySelector('input');
    input.focus();
    input.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true, cancelable: true }));
    assert.equal(fired, false);
  });

  for (const theme of ['light', 'dark']) {
    test(`the panel has no axe violations in ${theme}`, async () => {
      setTheme(theme);
      const el = await mountPanel();
      await assertNoA11yViolations(el);
    });
  }
});
