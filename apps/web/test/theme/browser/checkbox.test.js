/**
 * The checkbox's checked state, measured in a real browser in BOTH themes.
 *
 * This needs a browser and it needs the COMPILED stylesheet, because every part
 * of the bug it guards is invisible without both. The markup is identical in
 * either theme; what differs is which rule wins the cascade, and that is only
 * decided once Tailwind's output is parsed.
 *
 * The bug: the registry pairs `checked:bg-primary` with `dark:bg-input/30`.
 * Both are single-variant utilities, so on a dark page the later one won and a
 * CHECKED box kept the unchecked tint. Meanwhile the theme stylesheet chose the
 * near-black tick that belongs on a light primary fill. The result was a black
 * tick on a dark box: checked and unchecked told apart by nothing at all, which
 * is worse than the colour-only failure WCAG 1.4.1 already rules out.
 *
 * So this asserts the two halves AGREE, in both themes, rather than asserting a
 * specific colour: the fill has to be the primary token, and the tick has to be
 * the mark drawn for that fill.
 *
 * Counterfactuals: drop the `dark:checked:bg-primary` pair from
 * `components/ui/checkbox.ts` and the dark case fails on the fill; drop the
 * `:root[data-theme='dark']` tick rule from `public/input.css` and it fails on
 * the mark.
 */

const assert = {
  equal: (a, b, msg) => {
    if (a !== b) throw new Error(`${msg || 'Expected'}: got ${a}, want ${b}`);
  },
  ok: (v, msg) => {
    if (!v) throw new Error(msg || 'Expected truthy');
  },
};

const root = document.documentElement;

/** Inject the app's real compiled stylesheet; utilities do not exist without it. */
async function loadStylesheet() {
  if (document.getElementById('app-css')) return;
  const res = await fetch('/public/tailwind.css');
  assert.ok(res.ok, `the compiled stylesheet is served (run npm run css:build): ${res.status}`);
  const style = document.createElement('style');
  style.id = 'app-css';
  style.textContent = await res.text();
  document.head.appendChild(style);
}

/** The value of a design token as the browser currently resolves it. */
function token(name) {
  // Resolving a `light-dark()` custom property needs an element to paint it on,
  // since getPropertyValue hands back the unresolved function text.
  const probe = document.createElement('div');
  probe.style.color = `var(${name})`;
  document.body.appendChild(probe);
  const value = getComputedStyle(probe).color;
  probe.remove();
  return value;
}

function setTheme(theme) {
  if (theme === 'system') {
    delete root.dataset.theme;
    root.classList.remove('dark');
  } else {
    root.dataset.theme = theme;
    root.classList.toggle('dark', theme === 'dark');
  }
}

suite('checkbox in both themes', () => {
  let box;

  suiteSetup(async () => {
    await loadStylesheet();
  });

  setup(async () => {
    const { checkboxClass } = await import('../../../components/ui/checkbox.ts');
    box = document.createElement('input');
    box.type = 'checkbox';
    box.setAttribute('data-slot', 'checkbox');
    box.className = checkboxClass();
    box.checked = true;
    document.body.appendChild(box);
  });

  teardown(() => {
    setTheme('system');
    box.remove();
  });

  for (const theme of ['light', 'dark']) {
    test(`a checked box fills with the primary token in ${theme}`, () => {
      setTheme(theme);
      assert.equal(
        getComputedStyle(box).backgroundColor,
        token('--primary'),
        `in ${theme} a checked box must read as checked, not as the unchecked input tint`,
      );
    });

    test(`and carries the tick drawn for that fill in ${theme}`, () => {
      setTheme(theme);
      const image = getComputedStyle(box).backgroundImage;
      const fill = /fill='(\w+)'/.exec(image)?.[1];
      // The light theme's primary is dark, so its mark is white, and the dark
      // theme's primary is light, so its mark is black. Either way the mark has
      // to be the OPPOSITE of the fill it sits on.
      assert.equal(fill, theme === 'light' ? 'white' : 'black', `the tick in ${theme} has no contrast against the fill`);
    });
  }

  test('an unchecked box is not filled, so the two states differ by more than a tick', () => {
    setTheme('light');
    box.checked = false;
    assert.ok(
      getComputedStyle(box).backgroundColor !== token('--primary'),
      'an unchecked box must not paint the checked fill',
    );
  });
});
