/**
 * The root layout: the only file in this app that may write the document
 * shell. It owns the design tokens, the theme, and the app chrome.
 *
 * The nav, the org switcher and the sign-out form are plain markup and plain
 * forms, so they work with scripting off. The theme toggle is the one element
 * here that needs a browser, and it is the only JavaScript any page ships.
 */

import { html, asset, cspNonce } from '@webjsdev/core';
import type { LayoutProps } from '@webjsdev/core';
import { currentUser } from '#modules/auth/queries/current-user.server.ts';
import { listOrgs } from '#modules/orgs/queries/list-orgs.server.ts';
import { isSignedOut } from '#modules/auth/session.server.ts';
import { switchOrg } from '#modules/orgs/actions/switch-org.server.ts';
import { buttonClass } from '#components/ui/button.ts';
import { nativeSelectClass, nativeSelectIconClass, nativeSelectWrapperClass } from '#components/ui/native-select.ts';
import { cn } from '#lib/utils/cn.ts';
import '#components/theme-toggle.ts';

export const metadata = {
  title: { default: 'pilots', template: '%s · pilots' },
  icons: '/public/favicon.svg',
};

const NAV = [
  ['/machines', 'Machines'],
  ['/services', 'Services'],
  ['/volumes', 'Volumes'],
  ['/domains', 'Domains'],
  ['/usage', 'Usage'],
  ['/keys', 'Keys'],
  ['/org', 'Org'],
] as const;

export default async function RootLayout({ children, url }: LayoutProps) {
  const me = await currentUser();
  const listed = me ? await listOrgs() : [];
  const orgs = isSignedOut(listed) ? [] : listed;
  const path = new URL(url ?? 'http://localhost/').pathname;
  const nonce = cspNonce();

  return html`
    <script nonce="${nonce}">
      // Apply the saved theme before the first paint, so a reload of a page
      // chosen as dark does not flash light. The tokens below follow
      // color-scheme, which [data-theme] forces and otherwise inherits from the
      // OS, so an unset choice needs no work here beyond the .dark class the
      // kit's dark: variants key on. (No backticks in here: this comment is
      // inside a template literal, so one would end it.)
      (function () {
        try {
          var mq = window.matchMedia('(prefers-color-scheme: dark)');
          function apply() {
            var t = null;
            try { t = localStorage.getItem('pilots_theme'); } catch (_) {}
            var el = document.documentElement;
            if (t === 'light' || t === 'dark') el.dataset.theme = t;
            else delete el.dataset.theme;
            el.classList.toggle('dark', t === 'dark' || (t !== 'light' && mq.matches));
          }
          apply();
          mq.addEventListener('change', apply);
        } catch (_) {}
      })();
    </script>
    <meta name="color-scheme" content="light dark">
    <link rel="stylesheet" href=${asset('/public/tailwind.css')}>
    <style>
      /* Design tokens. The NAMES are infrastructure: public/input.css maps them
         into Tailwind utilities via @theme, which is where bg-card,
         text-muted-foreground and border-border come from. The VALUES are here,
         as plain custom properties, so they resolve with JavaScript disabled.

         One definition per colour via light-dark(LIGHT, DARK), so a palette
         change lands in one place. color-scheme decides which half applies:
         the default 'light dark' follows the OS and the [data-theme] rules
         below force one. That is why this block also overrides the flat :root
         and .dark palettes public/input.css ships: those are the kit's
         placeholder neutrals, and the toggle would otherwise have to keep two
         palettes agreeing with each other.

         light-dark() is COLOUR-only. A non-colour token that must differ per
         theme needs a :root[data-theme='dark'] rule plus a
         prefers-color-scheme media query; nothing here does. */
      :root {
        --font-sans: ui-sans-serif, system-ui, -apple-system, 'Segoe UI', Roboto, 'Helvetica Neue', sans-serif;
        --font-serif: ui-serif, Georgia, 'Times New Roman', serif;
        --font-mono: ui-monospace, SFMono-Regular, 'SF Mono', Menlo, Consolas, 'Liberation Mono', monospace;

        color-scheme: light dark;

        --background:           light-dark(#ffffff, #16181d);
        --foreground:           light-dark(#15181d, #e3e6ea);
        --card:                 light-dark(#f8f9fb, #1d2026);
        --card-foreground:      light-dark(#15181d, #e3e6ea);
        --popover:              light-dark(#ffffff, #1d2026);
        --popover-foreground:   light-dark(#15181d, #e3e6ea);
        --primary:              light-dark(#1f4fd8, #8ab0ff);
        --primary-foreground:   light-dark(#ffffff, #12141a);
        --secondary:            light-dark(#eef0f4, #262a31);
        --secondary-foreground: light-dark(#15181d, #e3e6ea);
        --muted:                light-dark(#f1f3f6, #22262d);
        --muted-foreground:     light-dark(#5a6270, #949cab);
        --accent:               light-dark(#e8ebf0, #2a2f37);
        --accent-foreground:    light-dark(#15181d, #e3e6ea);
        /* The dark half is lighter than a red normally wants to be, because
           this token is read as TEXT more often than it is painted as a fill:
           the kit's destructive alert renders its description in it against
           --card. At #e5484d that pair measured 3.3:1 and axe failed it. Where
           it IS a fill the kit already dims it (dark:bg-destructive/60), so the
           lighter value costs nothing there. */
        --destructive:          light-dark(#c0332b, #f87171);
        /* Text ON a filled destructive surface. The kit's own filled variants
           hard-code text-white, so this is what a text-destructive-foreground
           call site would resolve to, and it has to exist for that utility to
           render at all. (No backticks anywhere in this block: it is inside a
           template literal, so one would end it.) */
        --destructive-foreground: light-dark(#ffffff, #12141a);
        --border:               light-dark(#e1e4ea, #30353d);
        --border-strong:        light-dark(#c8cdd6, #3f454f);
        --input:                light-dark(#e1e4ea, #30353d);
        --ring:                 light-dark(#1f4fd8, #8ab0ff);
        /* A translucent primary, tracked across both themes for free because it
           derives from a token that already is. */
        --primary-tint: color-mix(in srgb, var(--primary) 22%, transparent);
      }
      /* The toggle writes data-theme to FORCE a scheme; with neither attribute
         the 'light dark' above follows the OS. */
      :root[data-theme='light'] { color-scheme: light; }
      :root[data-theme='dark'] { color-scheme: dark; }
    </style>
    <style>
      /* Base styles no utility class can reach. */
      html, body { margin: 0; }
      body {
        background: var(--background);
        color: var(--foreground);
        font: 15px/1.6 var(--font-sans);
        -webkit-font-smoothing: antialiased;
        -moz-osx-font-smoothing: grayscale;
      }
    </style>

    ${me
      ? html`
          <header class="border-b border-border">
            <div class="max-w-6xl mx-auto px-6 py-3 flex flex-wrap items-center gap-x-6 gap-y-3">
              <a href="/machines" class="font-semibold tracking-tight no-underline text-foreground">pilots</a>

              <nav class="flex items-center gap-4 text-sm" aria-label="Primary">
                ${NAV.map(
                  ([href, label]) => html`
                    <a
                      href=${href}
                      class=${cn(
                        'no-underline transition-colors',
                        path.startsWith(href) ? 'text-foreground font-medium' : 'text-muted-foreground hover:text-foreground',
                      )}
                      aria-current=${path.startsWith(href) ? 'page' : 'false'}
                      >${label}</a
                    >
                  `,
                )}
              </nav>

              <div class="ml-auto flex items-center gap-3 text-sm">
                ${orgs.length > 1
                  ? html`
                      <form action=${switchOrg} class="flex items-end gap-2">
                        <input type="hidden" name="back" value=${path}>
                        <label class="sr-only" for="org-switch">Organisation</label>
                        <div class=${nativeSelectWrapperClass()}>
                          <select id="org-switch" name="org" data-size="sm" class=${nativeSelectClass()}>
                            ${orgs.map(
                              (o) => html`<option value=${o.id} ?selected=${o.id === me.org.id}>${o.slug}</option>`,
                            )}
                          </select>
                          <svg class=${nativeSelectIconClass()} viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m6 9 6 6 6-6" /></svg>
                        </div>
                        <button type="submit" class=${buttonClass({ variant: 'outline', size: 'sm' })}>Switch</button>
                      </form>
                    `
                  : html`<span class="text-muted-foreground">${me.org.slug}</span>`}

                <span class="text-muted-foreground">${me.login}</span>
                <form method="POST" action="/api/auth/signout">
                  <button type="submit" class=${buttonClass({ variant: 'ghost', size: 'sm' })}>Sign out</button>
                </form>
                <theme-toggle></theme-toggle>
              </div>
            </div>
          </header>
        `
      : html`
          <header class="border-b border-border">
            <div class="max-w-6xl mx-auto px-6 py-3 flex items-center gap-4">
              <a href="/" class="font-semibold tracking-tight no-underline text-foreground">pilots</a>
              <div class="ml-auto"><theme-toggle></theme-toggle></div>
            </div>
          </header>
        `}

    <main class="max-w-6xl mx-auto px-6 py-8">${children}</main>
  `;
}
