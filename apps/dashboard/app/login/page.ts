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
import { alertClass, alertDescriptionClass, alertTitleClass } from '#components/ui/alert.ts';
import { cardClass, cardContentClass, cardHeaderClass, cardTitleClass } from '#components/ui/card.ts';

export const metadata = { title: 'Sign in' };

const MESSAGES: Record<string, string> = {
  AccessDenied: 'GitHub declined that sign-in.',
  Configuration: 'This dashboard has no GitHub App configured.',
};

export default function Login({ searchParams }: PageProps) {
  const error = String(searchParams.error ?? '');
  return html`
    <div class="max-w-md mx-auto py-24 grid gap-6">
      ${error
        ? html`
            <div role="alert" class=${alertClass({ variant: 'destructive' })}>
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
                <path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3Z" />
                <path d="M12 9v4M12 17h.01" />
              </svg>
              <div data-slot="alert-title" class=${alertTitleClass()}>Sign-in failed</div>
              <div data-slot="alert-description" class=${alertDescriptionClass()}>
                ${MESSAGES[error] ?? 'That sign-in did not complete.'}
              </div>
            </div>
          `
        : ''}

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
