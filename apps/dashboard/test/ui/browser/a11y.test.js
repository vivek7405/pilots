/**
 * Accessibility, via the framework's axe wrapper, on markup the app really
 * serves and against the palette it really paints.
 *
 * The scaffold ships `assertNoA11yViolations` wired into its starter browser
 * test and this app dropped the whole starter suite, so nothing here had ever
 * been run through axe. That mattered: the pages had unlabelled controls,
 * unscoped table headers and captionless tables, none of which any other check
 * in this repo looks at.
 *
 * Two things have to be real for this to mean anything, and both are set up
 * below rather than approximated:
 *
 *  - The MARKUP is fetched from the running app, not hand-written here, so a
 *    page that stops using the helpers is caught rather than a fixture that
 *    always uses them.
 *  - The STYLES are the app's own: the compiled stylesheet plus the token block
 *    the layout writes inline. Axe's contrast rule reads computed colour, so a
 *    page mounted without them is measured against black-on-white and passes no
 *    matter what the palette does.
 *
 * Both themes run, because a light pass proves nothing about dark: the tokens
 * swap sides of every `light-dark()` and the kit's `dark:` variants come in.
 *
 * Counterfactual: drop the `for`/`id` pair from `field()` in `lib/utils/ui.ts`
 * and the login and home pages still look right while these tests report an
 * unlabelled control.
 */

import { assertNoA11yViolations } from '@webjsdev/core/testing';

const assert = {
  ok: (v, msg) => {
    if (!v) throw new Error(msg || 'Expected truthy');
  },
};

/** Anonymous pages: the browser harness has no session to sign in with. */
const PAGES = ['/login?error=AccessDenied', '/'];

const root = document.documentElement;
let stylesLoaded = false;

/**
 * Mount one served page's body and its own `<style>` blocks into this document.
 *
 * The whole document cannot be swapped, so the page's head styles are lifted
 * across explicitly. Without the layout's inline token block every colour
 * resolves to nothing and axe measures the browser default.
 */
async function mountPage(path) {
  const res = await fetch(path);
  assert.ok(res.ok, `${path} is served: ${res.status}`);
  const doc = new DOMParser().parseFromString(await res.text(), 'text/html');

  if (!stylesLoaded) {
    const css = await fetch('/public/tailwind.css');
    assert.ok(css.ok, `the compiled stylesheet is served (run npm run css:build): ${css.status}`);
    const sheet = document.createElement('style');
    sheet.textContent = await css.text();
    document.head.appendChild(sheet);
    stylesLoaded = true;
  }
  for (const style of doc.querySelectorAll('style')) document.head.appendChild(style.cloneNode(true));

  const mount = document.createElement('div');
  mount.append(...doc.body.childNodes);
  document.body.appendChild(mount);
  // The layout paints the page background on <body>, and axe walks up to it to
  // resolve a contrast pair, so the harness body has to carry it too.
  document.body.style.background = 'var(--background)';
  document.body.style.color = 'var(--foreground)';
  return mount;
}

function setTheme(theme) {
  if (theme === 'light') {
    root.dataset.theme = 'light';
    root.classList.remove('dark');
  } else {
    root.dataset.theme = 'dark';
    root.classList.add('dark');
  }
}

suite('accessibility of the served pages', () => {
  teardown(() => {
    delete root.dataset.theme;
    root.classList.remove('dark');
    document.body.style.background = '';
    document.body.style.color = '';
    for (const el of document.querySelectorAll('body > div')) el.remove();
  });

  for (const path of PAGES) {
    for (const theme of ['light', 'dark']) {
      test(`${path} has no axe violations in ${theme}`, async () => {
        setTheme(theme);
        const mount = await mountPage(path);
        await assertNoA11yViolations(mount);
      });
    }
  }

  test('the login page really renders the alert being checked', async () => {
    setTheme('light');
    const mount = await mountPage('/login?error=AccessDenied');
    // A guard against a vacuous pass: axe on an empty div reports nothing.
    assert.ok(mount.querySelector('[role="alert"]'), 'the failed sign-in renders an alert');
    assert.ok(/GitHub declined/.test(mount.textContent), 'and says which refusal it was');
  });
});

/**
 * The signed-in surfaces, which the pages above cannot reach.
 *
 * Everything behind the session gate renders through two live components, and
 * between them they carry every badge the app can paint: the machine states,
 * and a host that is down. Those are the colour-on-colour pairs most likely to
 * fail, and they are exactly what a fetch of an anonymous page misses.
 *
 * Mounting the components rather than the pages is what makes that reachable
 * without a session, and it costs nothing in fidelity: the badge markup is the
 * component's, not a copy.
 */
suite('accessibility of the live components', () => {
  const RealWebSocket = globalThis.WebSocket;

  /** A socket that opens and never delivers: these tests are about the DOM. */
  class InertSocket {
    constructor(url) {
      this.url = url;
      this.readyState = 1;
      queueMicrotask(() => this.onopen?.({}));
    }
    send() {}
    close() {
      this.onclose = null;
    }
  }

  suiteSetup(async () => {
    // The token block lives on the served page, so borrow one to get a themed
    // document; the components are then mounted into it.
    await mountPage('/login');
  });

  setup(() => {
    globalThis.WebSocket = InertSocket;
  });

  teardown(() => {
    globalThis.WebSocket = RealWebSocket;
    delete root.dataset.theme;
    root.classList.remove('dark');
    for (const el of document.querySelectorAll('machine-list, hosts-strip')) el.remove();
  });

  const machine = (id, state) => ({ id, name: id, state, host_id: 'host-a1', url: `https://${id}.example.com` });

  for (const theme of ['light', 'dark']) {
    test(`every machine state badge is legible in ${theme}`, async () => {
      setTheme(theme);
      await import('../../../modules/machines/components/machine-list.ts');
      const el = document.createElement('machine-list');
      // Every state the badge has a colour for, plus one it does not, so the
      // fallback is measured too.
      el.initial = ['running', 'starting', 'suspended', 'stopped', 'destroyed', 'failed', 'rebooting'].map((s) =>
        machine(`m-${s}`, s),
      );
      document.body.appendChild(el);
      await el.updateComplete;
      assert.ok(el.querySelectorAll('tbody tr').length === 7, 'all seven rows render');
      await assertNoA11yViolations(el);
    });

    test(`an up and a down host are both legible in ${theme}`, async () => {
      setTheme(theme);
      await import('../../../modules/usage/components/hosts-strip.ts');
      const el = document.createElement('hosts-strip');
      el.initial = [
        { id: 'host-a1', alive: true, cpu_free: 12, mem_free_mib: 48000 },
        { id: 'host-b2', alive: false, cpu_free: 0, mem_free_mib: 0 },
      ];
      document.body.appendChild(el);
      await el.updateComplete;
      assert.ok(/down/.test(el.textContent), 'the down host renders, so its badge is under test');
      await assertNoA11yViolations(el);
    });
  }
});
