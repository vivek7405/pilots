/** Who is in the org, and the two things an owner can do about it. */
import { html } from '@webjsdev/core';
import type { PageProps } from '@webjsdev/core';
import { requireOrg } from '#modules/auth/session.server.ts';
import { listMembers } from '#modules/orgs/queries/list-members.server.ts';
import { inviteMember } from '#modules/orgs/actions/invite-member.server.ts';
import { removeMember } from '#modules/orgs/actions/remove-member.server.ts';

export const metadata = { title: 'Org' };

export default async function OrgPage({ actionData }: PageProps) {
  const ctx = (await requireOrg())!;
  const members = await listMembers(ctx.org.id);
  const result = (actionData as { error?: string; fieldErrors?: Record<string, string> } | undefined) ?? {};
  const isOwner = ctx.role === 'owner';

  return html`
    <h1 class="text-2xl font-semibold tracking-tight m-0">${ctx.org.name}</h1>
    <p class="text-muted-foreground mt-1 mb-6">
      ${ctx.org.personal ? 'Your personal org.' : 'A shared org.'} You are ${ctx.role === 'owner' ? 'an owner' : 'a member'}.
    </p>

    ${result.error ? html`<p role="alert" class="text-destructive">${result.error}</p>` : ''}

    <div class="overflow-x-auto mb-8">
      <table class="w-full text-sm border-collapse">
        <thead>
          <tr class="text-left text-muted-foreground border-b border-border">
            <th class="py-2 pr-4 font-medium">Member</th>
            <th class="py-2 pr-4 font-medium">Role</th>
            <th class="py-2 pr-4 font-medium">Since</th>
            <th class="py-2 font-medium"><span class="sr-only">Actions</span></th>
          </tr>
        </thead>
        <tbody>
          ${members.map(
            (m) => html`
              <tr class="border-b border-border">
                <td class="py-2 pr-4">
                  <a href=${`https://github.com/${m.login}`} rel="noopener">${m.login}</a>
                  ${m.name ? html` <span class="text-muted-foreground">${m.name}</span>` : ''}
                </td>
                <td class="py-2 pr-4">${m.role}</td>
                <td class="py-2 pr-4 text-muted-foreground">${m.since.toISOString().slice(0, 10)}</td>
                <td class="py-2 text-right">
                  ${isOwner && m.userId !== ctx.user.id
                    ? html`
                        <form action=${removeMember}>
                          <input type="hidden" name="user" value=${String(m.userId)}>
                          <button type="submit" class="rounded-md border border-border px-2 py-1 hover:bg-muted">
                            Remove
                          </button>
                        </form>
                      `
                    : ''}
                </td>
              </tr>
            `,
          )}
        </tbody>
      </table>
    </div>

    ${isOwner
      ? html`
          <h2 class="text-lg font-medium m-0 mb-2">Add a member</h2>
          <form action=${inviteMember} class="flex flex-wrap items-end gap-3">
            <label class="text-sm">
              <span class="block text-muted-foreground">GitHub username</span>
              <input name="login" placeholder="octocat" required class="mt-1 rounded-md border border-border bg-background px-2 py-1">
            </label>
            <button type="submit" class="rounded-md border border-border px-3 py-1.5 text-sm hover:bg-muted">Add</button>
          </form>
          ${result.fieldErrors?.login ? html`<p class="text-sm text-destructive">${result.fieldErrors.login}</p>` : ''}
          <p class="mt-3 text-xs text-muted-foreground">
            An owner cannot remove themselves: an org with no owner has nobody who can invite one back.
          </p>
        `
      : ''}
  `;
}
