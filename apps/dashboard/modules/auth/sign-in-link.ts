/**
 * The sign-in link, shared by the home page and the login page.
 *
 * A plain `<a>` and not a form: the handler turns a GET on this path into the
 * OAuth redirect, so it needs no body and works with scripting off.
 */
import { html } from '@webjsdev/core';
import type { TemplateResult } from '@webjsdev/core';

export function signInLink(): TemplateResult {
  return html`
    <a
      href="/api/auth/signin/github"
      class="inline-flex items-center gap-2 rounded-md bg-primary px-4 py-2 text-primary-foreground no-underline hover:opacity-90"
      >Sign in with GitHub</a
    >
  `;
}
