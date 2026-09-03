/**
 * The root layout: the only file in this app that may write the document
 * shell.
 *
 * It renders the nav, the org switcher and the sign-out form. All three are
 * plain markup and plain forms: a page never hydrates, so this costs the
 * browser no JavaScript at all, and the switcher works with scripting off.
 */

import { html, asset } from '@webjsdev/core';
import type { LayoutProps } from '@webjsdev/core';
import { currentUser } from '#modules/auth/queries/current-user.server.ts';
import { listOrgs } from '#modules/orgs/queries/list-orgs.server.ts';
import { switchOrg } from '#modules/orgs/actions/switch-org.server.ts';

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
  const orgs = me ? await listOrgs(me.id) : [];
  const path = new URL(url ?? 'http://localhost/').pathname;

  return html`
    <meta name="color-scheme" content="light dark">
    <link rel="stylesheet" href=${asset('/public/tailwind.css')}>
    <style>
      html, body { margin: 0; }
      body {
        background: var(--background, Canvas);
        color: var(--foreground, CanvasText);
        font: 15px/1.6 ui-sans-serif, system-ui, -apple-system, 'Segoe UI', Roboto, sans-serif;
        -webkit-font-smoothing: antialiased;
      }
    </style>

    ${me
      ? html`
          <header class="border-b border-border">
            <div class="max-w-6xl mx-auto px-6 py-3 flex flex-wrap items-center gap-x-6 gap-y-3">
              <a href="/machines" class="font-semibold tracking-tight no-underline text-foreground">pilots</a>

              <nav class="flex items-center gap-4 text-sm">
                ${NAV.map(
                  ([href, label]) => html`
                    <a
                      href=${href}
                      class=${path.startsWith(href)
                        ? 'no-underline text-foreground font-medium'
                        : 'no-underline text-muted-foreground hover:text-foreground'}
                      aria-current=${path.startsWith(href) ? 'page' : 'false'}
                      >${label}</a
                    >
                  `,
                )}
              </nav>

              <div class="ml-auto flex items-center gap-3 text-sm">
                ${orgs.length > 1
                  ? html`
                      <form action=${switchOrg} class="flex items-center gap-2">
                        <input type="hidden" name="back" value=${path}>
                        <label class="sr-only" for="org-switch">Org</label>
                        <select
                          id="org-switch"
                          name="org"
                          class="rounded-md border border-border bg-background px-2 py-1 text-sm"
                        >
                          ${orgs.map(
                            (o) => html`<option value=${o.id} ?selected=${o.id === me.org.id}>${o.slug}</option>`,
                          )}
                        </select>
                        <button type="submit" class="rounded-md border border-border px-2 py-1 hover:bg-muted">
                          Switch
                        </button>
                      </form>
                    `
                  : html`<span class="text-muted-foreground">${me.org.slug}</span>`}

                <span class="text-muted-foreground">${me.login}</span>
                <form method="POST" action="/api/auth/signout">
                  <button type="submit" class="rounded-md border border-border px-2 py-1 hover:bg-muted">
                    Sign out
                  </button>
                </form>
              </div>
            </div>
          </header>
        `
      : ''}

    <main class="max-w-6xl mx-auto px-6 py-8">${children}</main>
  `;
}
