/**
 * The sign-in page, and where a failed sign-in lands.
 *
 * `createAuth` is configured with `pages.error: '/login'`, so a refusal
 * arrives here as `?error=<code>` rather than at the home page, where it would
 * have looked like a successful sign-out.
 */
import { html } from '@webjsdev/core';
import type { PageProps } from '@webjsdev/core';
import { signInLink } from '#modules/auth/sign-in-link.ts';

export const metadata = { title: 'Sign in' };

const MESSAGES: Record<string, string> = {
  AccessDenied: 'GitHub declined that sign-in.',
  Configuration: 'This dashboard has no GitHub App configured.',
};

export default function Login({ searchParams }: PageProps) {
  const error = String(searchParams.error ?? '');
  return html`
    <div class="max-w-md mx-auto py-24 flex flex-col items-center gap-6 text-center">
      <h1 class="text-2xl font-semibold tracking-tight m-0">Sign in</h1>
      ${error
        ? html`<p role="alert" class="m-0 text-destructive">${MESSAGES[error] ?? 'That sign-in did not complete.'}</p>`
        : html`<p class="m-0 text-muted-foreground">A pilots account is a GitHub account.</p>`}
      ${signInLink()}
    </div>
  `;
}
