/**
 * Who is in the org, the two things an owner can do about it, and the account
 * actions the header's identity menu holds.
 *
 * The Account section is not a duplicate for its own sake. The identity menu is
 * a `<ui-dropdown-menu>`, whose panel is a `popover="manual"` element and so is
 * invisible with scripting off; these are the same two forms, on a real page,
 * for a visitor who has none.
 */
import { html } from '@webjsdev/core';
import type { PageProps } from '@webjsdev/core';
import { orUnauthorized, requireOrg, isSignedOut } from '#modules/auth/session.server.ts';
import { listOrgs } from '#modules/orgs/queries/list-orgs.server.ts';
import { switchOrg } from '#modules/orgs/actions/switch-org.server.ts';
import { nativeSelectClass, nativeSelectIconClass, nativeSelectWrapperClass } from '#components/ui/native-select.ts';
import { listMembers } from '#modules/orgs/queries/list-members.server.ts';
import type { MemberRow } from '#modules/orgs/queries/list-members.server.ts';
import { inviteMember } from '#modules/orgs/actions/invite-member.server.ts';
import { removeMember } from '#modules/orgs/actions/remove-member.server.ts';
import { badgeClass } from '#components/ui/badge.ts';
import { buttonClass } from '#components/ui/button.ts';
import { inputClass } from '#components/ui/input.ts';
import {
  dataTable,
  errorAlert,
  field,
  footnote,
  formRowClass,
  lede,
  pageHeading,
  sectionHeading,
} from '#lib/utils/ui.ts';

export const metadata = { title: 'Team' };

export default async function OrgPage({ actionData }: PageProps) {
  const ctx = (await requireOrg())!;
  const members = orUnauthorized(await listMembers());
  const listed = await listOrgs();
  const orgs = isSignedOut(listed) ? [] : listed;
  const result = (actionData as { error?: string; fieldErrors?: Record<string, string> } | undefined) ?? {};
  const isOwner = ctx.role === 'owner';

  return html`
    ${pageHeading(ctx.org.name)}
    ${lede(html`${ctx.org.personal ? 'Your personal org.' : 'A shared org.'} You are
    ${ctx.role === 'owner' ? 'an owner' : 'a member'}.`)}
    ${result.error ? errorAlert(result.error) : ''}

    <div class="mb-8">
      ${dataTable<MemberRow>({
        caption: 'Members of this organisation',
        rows: members,
        columns: [
          {
            header: 'Member',
            cell: (m) => html`
              <a href=${`https://github.com/${m.login}`} rel="noopener">${m.login}</a>
              ${m.name ? html` <span class="text-muted-foreground">${m.name}</span>` : ''}
            `,
          },
          {
            header: 'Role',
            cell: (m) =>
              html`<span class=${badgeClass({ variant: m.role === 'owner' ? 'default' : 'secondary' })}>${m.role}</span>`,
          },
          {
            header: 'Since',
            cellClass: 'text-muted-foreground tabular-nums',
            cell: (m) => m.since.toISOString().slice(0, 10),
          },
          {
            header: 'Actions',
            headerHidden: true,
            align: 'right',
            cell: (m) =>
              isOwner && m.userId !== ctx.user.id
                ? html`
                    <form action=${removeMember}>
                      <input type="hidden" name="user" value=${String(m.userId)}>
                      <button type="submit" class=${buttonClass({ variant: 'outline', size: 'sm' })}>Remove</button>
                    </form>
                  `
                : '',
          },
        ],
      })}
    </div>

    ${isOwner
      ? html`
          ${sectionHeading('Add a member')}
          <form action=${inviteMember} class=${formRowClass()}>
            ${field({
              id: 'login',
              label: 'GitHub username',
              error: result.fieldErrors?.login,
              control: html`<input
                id="login"
                name="login"
                placeholder="octocat"
                required
                aria-invalid=${result.fieldErrors?.login ? 'true' : 'false'}
                class=${inputClass()}
              >`,
            })}
            <button type="submit" class=${buttonClass()}>Add</button>
          </form>
          ${footnote('An owner cannot remove themselves: an org with no owner has nobody who can invite one back.')}
        `
      : ''}

    <div class="mt-10 pt-8 border-t border-border">
      ${sectionHeading('Account')}
      ${lede('The same actions the account menu in the header holds. They live here too so they work with scripting off.')}
      <div class="flex flex-wrap items-end gap-6">
        ${orgs.length > 1
          ? html`
              <form action=${switchOrg} class=${formRowClass()}>
                <input type="hidden" name="back" value="/org">
                ${field({
                  id: 'org-switch',
                  label: 'Organisation',
                  control: html`
                    <div class=${nativeSelectWrapperClass()}>
                      <select id="org-switch" name="org" data-size="sm" class=${nativeSelectClass()}>
                        ${orgs.map((o) => html`<option value=${o.id} ?selected=${o.id === ctx.org.id}>${o.slug}</option>`)}
                      </select>
                      <svg class=${nativeSelectIconClass()} viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m6 9 6 6 6-6" /></svg>
                    </div>
                  `,
                })}
                <button type="submit" class=${buttonClass({ variant: 'outline', size: 'sm' })}>Switch</button>
              </form>
            `
          : ''}
        <form method="POST" action="/api/auth/signout">
          <button type="submit" class=${buttonClass({ variant: 'outline', size: 'sm' })}>Sign out</button>
        </form>
      </div>
    </div>
  `;
}
