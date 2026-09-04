/** Signed in, go to the machines list. Signed out, offer the one way in. */
import { html, redirect } from '@webjsdev/core';
import { currentUser } from '#modules/auth/queries/current-user.server.ts';
import { signInLink } from '#modules/auth/sign-in-link.ts';
import { badgeClass } from '#components/ui/badge.ts';

export const metadata = { title: 'pilots' };

export default async function Home() {
  if (await currentUser()) throw redirect('/machines');
  return html`
    <div class="max-w-md mx-auto py-24 flex flex-col items-center gap-6 text-center">
      <span class=${badgeClass({ variant: 'outline' })}>Firecracker microVMs</span>
      <h1 class="text-3xl font-semibold tracking-tight m-0">pilots</h1>
      <p class="text-muted-foreground m-0">Sandboxes and production services on one primitive.</p>
      ${signInLink()}
    </div>
  `;
}
