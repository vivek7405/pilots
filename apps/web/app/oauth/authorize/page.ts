/**
 * The consent screen: the one place a human decides what an agent may do.
 *
 * Everything else in this flow is mechanical. This page is where the product's
 * actual security boundary is drawn, so it says four things plainly and asks
 * for a deliberate act: which application is asking, which team the token will
 * belong to, what it will be allowed to do, and what it will NOT be allowed to
 * touch. The restriction fields are pre-filled with the tight answer rather
 * than the permissive one, because a default nobody changes is the setting
 * everybody has.
 *
 * Every control here is enforced by the engine: the prefix, the cap and the
 * expiry are stored with the token and checked on the request path. That is
 * the rule this app holds itself to -- no control for something nothing
 * reads -- and it is why these three arrived with their enforcement rather
 * than before it.
 *
 * A visitor with no session is sent to sign in and comes back here, which is
 * why the whole request is carried on `?next=`.
 */

import { html } from '@webjsdev/core';
import type { PageProps } from '@webjsdev/core';

import { orUnauthorized, requireOrg } from '#modules/auth/session.server.ts';
import { signInLink } from '#modules/auth/sign-in-link.ts';
import { listOrgs } from '#modules/orgs/queries/list-orgs.server.ts';
import { badgeClass } from '#components/ui/badge.ts';
import { buttonClass } from '#components/ui/button.ts';
import { cardClass, cardContentClass, cardHeaderClass, cardTitleClass } from '#components/ui/card.ts';
import { inputClass } from '#components/ui/input.ts';
import { labelClass } from '#components/ui/label.ts';
import { errorAlert, footnote } from '#lib/utils/ui.ts';
import { NOUN } from '#lib/vocabulary.ts';
import { cn } from '#lib/utils/cn.ts';
import { defaultPrefixFor, errorRedirect, parseAuthorize, signTicket } from '#modules/oauth/authorize.server.ts';

export const metadata = { title: 'Authorize' };

/** The lifetimes the screen offers, in hours. `0` is "until revoked". */
const LIFETIMES: { value: number; label: string }[] = [
  { value: 0, label: 'Until I revoke it' },
  { value: 24, label: '24 hours' },
  { value: 24 * 7, label: '7 days' },
  { value: 24 * 30, label: '30 days' },
];

export default async function Authorize({ url }: PageProps) {
  // The query is read from the URL rather than from `searchParams`, because
  // the exact string is what gets signed into the consent ticket and what a
  // sign-in round trip has to carry back verbatim.
  const params = new URL(url).searchParams;

  const parsed = await parseAuthorize(params);

  // A refusal the CLIENT should see goes back to its own verified redirect
  // URI. It is rendered as a link plus a meta refresh rather than a framework
  // redirect, because the target is off-origin by definition.
  if ('kind' in parsed && parsed.kind === 'client') {
    const target = errorRedirect(parsed);
    return html`
      <meta http-equiv="refresh" content="0;url=${target}" />
      <div class="max-w-md mx-auto py-24 grid gap-4">
        ${errorAlert(`${parsed.code}: ${parsed.description}`)}
        <a class=${buttonClass()} href=${target}>Return to the application</a>
      </div>
    `;
  }

  // A refusal with no verified place to send it stops here, on purpose: the
  // only address available would be the unvalidated one in the request.
  if ('kind' in parsed && parsed.kind === 'user') {
    return html`
      <div class="max-w-md mx-auto py-24 grid gap-4">
        ${errorAlert(parsed.title)}
        <p class="text-meta text-muted-foreground m-0">${parsed.detail}</p>
        <a class=${buttonClass({ variant: 'outline' })} href="/keys">Your ${NOUN.Tokens.toLowerCase()}</a>
      </div>
    `;
  }

  const ctx = await requireOrg();
  if (!ctx) {
    const next = `/oauth/authorize?${params.toString()}`;
    return html`
      <div class="max-w-md mx-auto py-24 grid gap-6">
        <div class=${cardClass()} data-slot="card">
          <div class=${cardHeaderClass()}>
            <h1 class=${cardTitleClass()}>Sign in to continue</h1>
            <p class="text-meta text-muted-foreground m-0">
              ${parsed.client.name} is asking for access to a pilots ${NOUN.Team.toLowerCase()}.
            </p>
          </div>
          <div class=${cardContentClass()}>${signInLink(next)}</div>
        </div>
      </div>
    `;
  }

  const orgs = orUnauthorized(await listOrgs());
  const ticket = signTicket(parsed, ctx.user.id);
  const prefix = defaultPrefixFor(parsed.scopes);
  const scopeText: Record<string, string> = {
    machines: `open ${NOUN.Sandboxes.toLowerCase()}, run commands in them, take ${NOUN.Snapshots.toLowerCase()} and delete them`,
    deploy: `everything above, plus build ${NOUN.Image.toLowerCase()+'s'}, ship ${NOUN.Deployments.toLowerCase()} and change what is serving`,
    admin: `everything above, plus create and revoke ${NOUN.Tokens.toLowerCase()} for every ${NOUN.Team.toLowerCase()}`,
  };

  return html`
    <div class="max-w-lg mx-auto py-16 grid gap-6">
      <div class=${cardClass()} data-slot="card">
        <div class=${cardHeaderClass()}>
          <h1 class=${cardTitleClass()}>Authorize ${parsed.client.name}</h1>
          <p class="text-meta text-muted-foreground m-0">
            It will get a pilots token. Nothing else about your GitHub account is shared.
            ${parsed.client.uri ? html` <a class="underline" href=${parsed.client.uri} rel="noreferrer noopener" target="_blank">${parsed.client.uri}</a>` : ''}
          </p>
        </div>

        <div class=${cardContentClass()}>
          <form method="POST" action="/api/oauth/approve" class="grid gap-5">
            <input type="hidden" name="ticket" value=${ticket} />

            <div class="grid gap-2">
              <span class=${labelClass()}>It will be able to</span>
              <ul class="m-0 pl-5 text-meta grid gap-1">
                ${parsed.scopes.map(
                  (scope) => html`<li><span class=${badgeClass()}>${scope}</span> ${scopeText[scope] ?? scope}</li>`,
                )}
              </ul>
            </div>

            <div class="grid gap-2">
              <label class=${labelClass()} for="org">For the ${NOUN.Team.toLowerCase()}</label>
              <select id="org" name="org" class=${cn(inputClass(), 'bg-background')}>
                ${orgs.map(
                  (org) => html`
                    <option value=${org.id} ?selected=${org.id === ctx.org.id}>${org.name}</option>
                  `,
                )}
              </select>
            </div>

            <div class="grid gap-2">
              <label class=${labelClass()} for="prefix">Only names starting with</label>
              <input id="prefix" name="prefix" class=${inputClass()} value=${prefix} placeholder="mcp-" autocomplete="off" />
              <p class="text-meta text-muted-foreground m-0">
                A prefix keeps this application to what it opens itself. Leave it empty to give it
                everything this ${NOUN.Team.toLowerCase()} has.
              </p>
            </div>

            <div class="grid gap-2 mid:grid-cols-2">
              <div class="grid gap-2">
                <label class=${labelClass()} for="max_machines">At most</label>
                <input id="max_machines" name="max_machines" class=${inputClass()} type="number" min="1" max="1000"
                       value="10" placeholder="10" />
                <p class="text-meta text-muted-foreground m-0">
                  open at once with that prefix. Empty means no limit.
                </p>
              </div>
              <div class="grid gap-2">
                <label class=${labelClass()} for="lifetime">Expires</label>
                <select id="lifetime" name="lifetime" class=${cn(inputClass(), 'bg-background')}>
                  ${LIFETIMES.map((l) => html`<option value=${String(l.value)}>${l.label}</option>`)}
                </select>
              </div>
            </div>

            <div class="flex gap-3">
              <button type="submit" name="decision" value="approve" class=${buttonClass()}>Authorize</button>
              <button type="submit" name="decision" value="deny" class=${buttonClass({ variant: 'outline' })}>
                Cancel
              </button>
            </div>
          </form>
        </div>
      </div>

      ${footnote(
        `This is an ordinary pilots ${NOUN.Token.toLowerCase()}. It appears on your ${NOUN.Tokens.toLowerCase()} page, and revoking it there stops it everywhere within seconds.`,
      )}
    </div>
  `;
}
