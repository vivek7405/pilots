/**
 * Create a team.
 *
 * A page rather than a dialog in the header: creating a team is the one act
 * that changes which account everything else on the screen belongs to, so it
 * gets a URL a person can land on, share and come back to, and it works with
 * scripting off like every other chrome action in this app.
 *
 * The list of what a team gives you is the empty state. There is nothing to
 * show yet by definition, and a bare name field over a Create button assumes
 * the reader already knows why they would want a second one.
 */
import { html } from '@webjsdev/core';
import type { PageProps } from '@webjsdev/core';
import { requireOrg } from '#modules/auth/session.server.ts';
import { createOrg } from '#modules/orgs/actions/create-org.server.ts';
import { buttonClass } from '#components/ui/button.ts';
import { cardClass } from '#components/ui/card.ts';
import { cn } from '#lib/utils/cn.ts';
import { cardBody, errorAlert, lede, pageHeading } from '#lib/utils/ui.ts';
import { NOUN } from '#lib/vocabulary.ts';
import '#modules/orgs/components/team-name-field.ts';

export const metadata = { title: 'New team' };

/** What a team is for, in the words of what it changes for the reader. */
const INCLUDES: { title: string; detail: string }[] = [
  {
    title: 'Shared ownership',
    detail: `Everything the team deploys belongs to the team, not to whoever pressed Deploy. People come and go without taking ${NOUN.Apps.toLowerCase()} with them.`,
  },
  {
    title: 'Its own tokens',
    detail: `${NOUN.Tokens} minted for a team are revoked with the team. An agent or a CI job gets one that reaches nothing else you own.`,
  },
  {
    title: 'Its own limits and plan',
    detail: 'A team carries its own ceilings, so one team cannot spend another one out of capacity.',
  },
  {
    title: 'Roles',
    detail: 'An owner runs the team; an admin adds and removes people; a member uses what the team owns.',
  },
];

export default async function NewOrgPage({ actionData }: PageProps) {
  // The page is behind the segment gate, so this is never null in practice;
  // it is read for the same reason every page in this segment reads it, to
  // fail closed if that ever stops being true.
  const ctx = (await requireOrg())!;
  const result = (actionData as { error?: string; fieldErrors?: Record<string, string> } | undefined) ?? {};

  return html`
    ${pageHeading('Create a team')}
    ${lede(
      html`A second place to put work, separate from <strong>${ctx.org.slug}</strong>. You will be its owner, and you
        can invite people once it exists.`,
    )}
    ${result.error ? errorAlert(result.error) : ''}

    <form action=${createOrg} class="grid gap-5 max-w-md">
      <team-name-field .error=${result.fieldErrors?.name ?? ''}></team-name-field>
      <div class="flex items-center gap-3">
        <button type="submit" class=${buttonClass()}>Create team</button>
        <a href="/org" class=${buttonClass({ variant: 'ghost', size: 'sm' })}>Cancel</a>
      </div>
    </form>

    <div class="mt-10 pt-8 border-t border-border">
      <h2 class="text-heading font-medium m-0 mb-3">What a team gives you</h2>
      <div class="grid gap-3 sm:grid-cols-2">
        ${INCLUDES.map(
          (item) => html`
            <div class=${cn(cardClass(), cardBody(), 'items-start gap-1')}>
              <p class="m-0 text-body font-medium">${item.title}</p>
              <p class="m-0 text-meta text-muted-foreground">${item.detail}</p>
            </div>
          `,
        )}
      </div>
    </div>
  `;
}
