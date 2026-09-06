/**
 * The root layout: the only file in this app that may write the document
 * shell. It owns the design tokens, the theme, and the app chrome.
 *
 * Everything the chrome can do works with scripting off. The org switch and
 * the sign out are real forms, and the identity menu that holds them is an
 * upgrade rather than the only way in: both forms are also on /org under
 * Account, and a <noscript> link points there.
 *
 * The header is `position: fixed`, never `sticky`. Sticky flickers its
 * background for one frame on iOS WebKit during a client-router navigation,
 * and every iOS browser is WebKit. A fixed header leaves normal flow, so its
 * height is reserved on the body through --header-h, which the pre-paint
 * script below keeps exact.
 */

import { html, asset, cspNonce } from '@webjsdev/core';
import type { LayoutProps } from '@webjsdev/core';
import { currentUser } from '#modules/auth/queries/current-user.server.ts';
import { listOrgs } from '#modules/orgs/queries/list-orgs.server.ts';
import { isSignedOut } from '#modules/auth/session.server.ts';
import { switchOrg } from '#modules/orgs/actions/switch-org.server.ts';
import { buttonClass } from '#components/ui/button.ts';
import { avatarClass, avatarFallbackClass, avatarImageClass } from '#components/ui/avatar.ts';
import { initials } from '#lib/utils/ui.ts';
import { breadcrumb } from '#lib/utils/breadcrumb.ts';
import { cn } from '#lib/utils/cn.ts';
import '#components/theme-toggle.ts';
import '#components/app-nav.ts';
import '#components/org-switcher.ts';
import '#components/flash-toast.ts';
import '#components/command-palette.ts';
import '#components/ui/dropdown-menu.ts';
import '#components/ui/sonner.ts';

export const metadata = {
  title: { default: 'pilots', template: '%s · pilots' },
  icons: '/public/favicon.svg',
};

export default async function RootLayout({ children, url }: LayoutProps) {
  const me = await currentUser();
  const listed = me ? await listOrgs() : [];
  const orgs = isSignedOut(listed) ? [] : listed;
  const path = new URL(url ?? 'http://localhost/').pathname;
  // Derived from the path alone. See lib/utils/breadcrumb.ts for why it cannot
  // read the page's data and what that settles about an id in the URL.
  const crumbs = breadcrumb(path);
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
      // The header is fixed, so it leaves normal flow and its height has to be
      // reserved on the content below. --header-h carries a sane SSR default
      // and this keeps it exact as the header reflows: a wrapped nav on a
      // narrow viewport makes it taller, and the first row of content would
      // otherwise disappear underneath it.
      (function () {
        function measure() {
          try {
            var hdr = document.querySelector('header');
            if (!hdr || getComputedStyle(hdr).position !== 'fixed') return;
            var apply = function () {
              document.documentElement.style.setProperty('--header-h', hdr.offsetHeight + 'px');
            };
            apply();
            if (window.ResizeObserver) new ResizeObserver(apply).observe(hdr);
          } catch (_) {}
        }
        if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', measure);
        else measure();
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

        /* The fixed header's height, reserved on the body below. A plain value
           so a first paint with no JavaScript is already right; the script
           above only corrects it when the header wraps. */
        --header-h: 56px;

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
        /* Status colours, for a toast and for anything else that reports an
           outcome. They exist because the kit's sonner shipped raw Tailwind
           swatches, which are the same in both themes and answer to no palette
           change. Mapped into utilities in public/input.css. */
        --success:              light-dark(#137a4a, #4ade80);
        --warning:              light-dark(#9a5a04, #fbbf24);
        --info:                 light-dark(#1d6fa5, #67c4f5);
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
      /* A short page and a tall one otherwise differ by a scrollbar's width,
         which slides the centred header content sideways on every navigation.
         The kit's dialog scroll lock defers to this declaration, so the two
         never double-compensate. */
      html { scrollbar-gutter: stable; }
      /* Two custom elements are a run of TEXT inside a sentence, not a block.
         An element with no display of its own is not reliably inline once the
         framework has upgraded it, and the visible symptom is small: a status
         line reading "deployed" and "1 hour ago" on two lines. Declared here
         because a light-DOM component cannot set the display of its own host,
         and only the layout may write document-level CSS. */
      relative-time { display: inline; }
      copy-button { display: inline-block; vertical-align: middle; }
      /* A tooltip wraps its trigger, so it has to take the trigger's place in
         the line: as a block it puts every row action on its own line and
         breaks a sentence in half around an explained word. The content
         element is a popover in the top layer and is positioned regardless. */
      ui-tooltip, ui-tooltip-trigger { display: inline-block; vertical-align: middle; }
      body {
        padding-top: var(--header-h);
        background: var(--background);
        color: var(--foreground);
        font: 15px/1.6 var(--font-sans);
        -webkit-font-smoothing: antialiased;
        -moz-osx-font-smoothing: grayscale;
      }
    </style>

    ${me
      ? html`
          <header
            class="fixed inset-x-0 top-0 z-40 border-b border-border bg-card/85 backdrop-blur"
            style="border-right: var(--wj-scrollbar-compensation, 0px) solid transparent"
          >
            <div class="max-w-6xl mx-auto px-6 py-3 flex flex-wrap items-center gap-x-6 gap-y-3">
              <nav aria-label="Breadcrumb" class="shrink-0 min-w-0">
                <ol class="flex items-center gap-1.5 list-none m-0 p-0 text-meta">
                  ${crumbs.map(
                    (crumb, i) => html`
                      ${i === 0 ? '' : html`<li aria-hidden="true" class="text-muted-foreground">/</li>`}
                      <li class="min-w-0 truncate">
                        ${crumb.href
                          ? html`<a
                              href=${crumb.href}
                              class=${cn(
                                'no-underline',
                                i === 0 ? 'font-semibold tracking-tight text-foreground' : 'text-muted-foreground',
                              )}
                              >${crumb.label}</a
                            >`
                          : html`<span
                              aria-current="page"
                              class=${cn('font-medium', i === 0 ? 'font-semibold tracking-tight' : '')}
                              >${crumb.label}</span
                            >`}
                      </li>
                    `,
                  )}
                </ol>
              </nav>

              <app-nav current=${path} class="min-w-0 flex-1"></app-nav>

              <div class="ml-auto flex items-center gap-2 text-meta">
                <command-palette></command-palette>
                <theme-toggle></theme-toggle>

                <org-switcher>
                  <ui-dropdown-menu>
                    <ui-dropdown-menu-trigger>
                      <button
                        type="button"
                        class=${cn(buttonClass({ variant: 'ghost', size: 'sm' }), 'gap-2')}
                        aria-label=${`Account: ${me.login}`}
                      >
                        <span class=${avatarClass({ size: 'sm' })} data-slot="avatar" data-size="sm">
                          ${me.avatarUrl
                            ? html`<img class=${avatarImageClass()} src=${me.avatarUrl} alt="">`
                            : html`<span class=${avatarFallbackClass()}>${initials(me.login)}</span>`}
                        </span>
                        <span>${me.login}</span>
                        ${me.org.personal
                          ? ''
                          : html`<span class="text-muted-foreground">${me.org.slug}</span>`}
                      </button>
                    </ui-dropdown-menu-trigger>
                    <ui-dropdown-menu-content align="end">
                      <ui-dropdown-menu-label>Signed in as ${me.login}</ui-dropdown-menu-label>
                      <ui-dropdown-menu-separator></ui-dropdown-menu-separator>
                      ${orgs.length > 1
                        ? html`
                            <ui-dropdown-menu-group aria-label="Team">
                              ${orgs.map(
                                (o) => html`<ui-dropdown-menu-item type="radio" value=${o.id} ?checked=${o.id === me.org.id}
                                  >${o.slug}</ui-dropdown-menu-item
                                >`,
                              )}
                            </ui-dropdown-menu-group>
                            <ui-dropdown-menu-separator></ui-dropdown-menu-separator>
                          `
                        : ''}
                      <ui-dropdown-menu-item><a href="/usage" class="no-underline text-foreground">Usage</a></ui-dropdown-menu-item>
                      <ui-dropdown-menu-item><a href="/keys" class="no-underline text-foreground">Tokens</a></ui-dropdown-menu-item>
                      <ui-dropdown-menu-item><a href="/org" class="no-underline text-foreground">Team</a></ui-dropdown-menu-item>
                      <ui-dropdown-menu-separator></ui-dropdown-menu-separator>
                      <ui-dropdown-menu-item variant="destructive">
                        <button type="submit" form="signout" class="w-full text-left bg-transparent border-0 p-0 font-inherit text-inherit cursor-pointer">Sign out</button>
                      </ui-dropdown-menu-item>
                    </ui-dropdown-menu-content>
                  </ui-dropdown-menu>

                  <!-- The radio items cannot post anything on their own, so
                       <org-switcher> fills these in and submits. The same form
                       is on /org under Account for a visitor with no JS. -->
                  <form action=${switchOrg} data-org-switch class="hidden">
                    <input type="hidden" name="org" value=${me.org.id}>
                    <input type="hidden" name="back" value=${path}>
                  </form>
                </org-switcher>

                <form id="signout" method="POST" action="/api/auth/signout" class="hidden"></form>
                <noscript><a href="/org" class="text-muted-foreground">Account</a></noscript>
              </div>
            </div>
          </header>
        `
      : html`
          <header
            class="fixed inset-x-0 top-0 z-40 border-b border-border bg-card/85 backdrop-blur"
            style="border-right: var(--wj-scrollbar-compensation, 0px) solid transparent"
          >
            <div class="max-w-6xl mx-auto px-6 py-3 flex items-center gap-4">
              <a href="/" class="font-semibold tracking-tight no-underline text-foreground">pilots</a>
              <div class="ml-auto"><theme-toggle></theme-toggle></div>
            </div>
          </header>
        `}

    <main class="max-w-6xl mx-auto px-6 py-8">${children}</main>
    <ui-sonner position="bottom-right"></ui-sonner>
    <flash-toast></flash-toast>
  `;
}
