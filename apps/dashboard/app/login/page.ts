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
import { cardClass, cardContentClass, cardHeaderClass, cardTitleClass } from '#components/ui/card.ts';
import { errorAlert } from '#lib/utils/ui.ts';

export const metadata = { title: 'Sign in' };

const MESSAGES: Record<string, string> = {
  AccessDenied: 'GitHub declined that sign-in.',
  Configuration: 'This dashboard has no GitHub App configured.',
};

export default function Login({ searchParams }: PageProps) {
  const error = String(searchParams.error ?? '');
  return html`
    <div class="max-w-md mx-auto py-24 grid gap-6">
      ${error ? errorAlert(MESSAGES[error] ?? 'That sign-in did not complete.') : ''}

      <div class=${cardClass()} data-slot="card">
        <div class=${cardHeaderClass()}>
          <h1 class=${cardTitleClass()}>Sign in</h1>
          <p class="text-sm text-muted-foreground m-0">A pilots account is a GitHub account.</p>
        </div>
        <div class=${cardContentClass()}>${signInLink()}</div>
      </div>
    </div>
  `;
}
