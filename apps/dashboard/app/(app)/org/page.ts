/** Who is in the org, and the two things an owner can do about it. */
import { html } from '@webjsdev/core';
import type { PageProps } from '@webjsdev/core';
import { requireOrg } from '#modules/auth/session.server.ts';
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

export const metadata = { title: 'Org' };

export default async function OrgPage({ actionData }: PageProps) {
  const ctx = (await requireOrg())!;
  const members = await listMembers(ctx.org.id);
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
  `;
}
