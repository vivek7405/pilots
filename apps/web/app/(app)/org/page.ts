/**
 * Everything about one team: who is in it, what it is called, what it is
 * allowed to run, what builds for it, and the account actions the header's
 * identity menu holds.
 *
 * The Account section is not a duplicate for its own sake. The identity menu is
 * a `<ui-dropdown-menu>`, whose panel is a `popover="manual"` element and so is
 * invisible with scripting off; these are the same two forms, on a real page,
 * for a visitor who has none.
 *
 * The dangerous half -- hand over, leave, delete -- sits at the BOTTOM, behind
 * its own rule, and every one of those forms is a plain form. An action that
 * ends a team must not live inside an overlay a visitor with no JavaScript
 * cannot open, and must not sit next to the button that adds a member.
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
import { renameOrg } from '#modules/orgs/actions/rename-org.server.ts';
import { leaveOrg } from '#modules/orgs/actions/leave-org.server.ts';
import { transferOwnership } from '#modules/orgs/actions/transfer-ownership.server.ts';
import { deleteOrg } from '#modules/orgs/actions/delete-org.server.ts';
import { canAdministerOrg, canManageMembers, roleLabel } from '#modules/orgs/roles.ts';
import { getBilling } from '#modules/billing/queries/get-billing.server.ts';
import type { BillingView } from '#modules/billing/queries/get-billing.server.ts';
import { activatePlan } from '#modules/billing/actions/activate-plan.server.ts';
import { PLANS, PLAN_IDS } from '#modules/billing/plans.ts';
import type { Plan } from '#modules/billing/plans.ts';
import { listBuilders } from '#modules/builders/queries/list-builders.server.ts';
import type { Builder } from '#modules/builders/types.ts';
import { resetBuilder } from '#modules/builders/actions/reset-builder.server.ts';
// `when` renders a fleet stamp as a relative time, and `<relative-time>`
// settles the unit through `epochMs` -- hostd stamps SECONDS, and a raw
// `new Date(last_activity)` lands in January 1970.
import { when } from '#modules/machines/utils/ui/status-line.ts';
import { stateLabel, NOUN } from '#lib/vocabulary.ts';
import { badgeClass } from '#components/ui/badge.ts';
import '#components/relative-time.ts';
import { buttonClass } from '#components/ui/button.ts';
import { cardClass } from '#components/ui/card.ts';
import { inputClass } from '#components/ui/input.ts';
import { cn } from '#lib/utils/cn.ts';
import {
  cardBody,
  dataTable,
  errorAlert,
  field,
  footnote,
  formRowClass,
  lede,
  pageHeading,
  sectionEmpty,
  sectionHeading,
} from '#lib/utils/ui.ts';

export const metadata = { title: 'Team' };

export default async function OrgPage({ actionData }: PageProps) {
  const ctx = (await requireOrg())!;
  const members = orUnauthorized(await listMembers());
  const listed = await listOrgs();
  const orgs = isSignedOut(listed) ? [] : listed;
  const billed = await getBilling();
  const billing = isSignedOut(billed) ? null : billed;
  const built = await listBuilders();
  const builders = isSignedOut(built) ? [] : built;
  const result = (actionData as { error?: string; fieldErrors?: Record<string, string> } | undefined) ?? {};
  const isOwner = canAdministerOrg(ctx.role);
  const manages = canManageMembers(ctx.role);
  const others = members.filter((m) => m.userId !== ctx.user.id);

  return html`
    ${pageHeading(ctx.org.name)}
    ${ctx.org.personal ? '' : html`<span class="sr-only">Team</span>`}
    ${lede(html`${ctx.org.personal ? 'Your personal team.' : 'A shared team.'} You are
    ${ctx.role === 'member' ? 'a member' : `an ${roleLabel(ctx.role).toLowerCase()}`}.`)}
    ${result.error ? errorAlert(result.error) : ''}

    <div class="mb-8">
      ${dataTable<MemberRow>({
        caption: 'Members of this team',
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
              html`<span class=${badgeClass({ variant: m.role === 'owner' ? 'default' : m.role === 'admin' ? 'outline' : 'secondary' })}
                >${roleLabel(m.role)}</span
              >`,
          },
          {
            // One column with one value today, and that is the point: this app
            // has exactly one way in, and a reader should not have to guess
            // whether some other one exists.
            header: 'Auth',
            cellClass: 'text-muted-foreground',
            cell: () => 'GitHub',
          },
          {
            header: 'Since',
            cellClass: 'text-muted-foreground',
            cell: (m) => html`<relative-time datetime=${m.since.toISOString()}></relative-time>`,
          },
          {
            header: 'Actions',
            headerHidden: true,
            align: 'right',
            cell: (m) =>
              manages && m.userId !== ctx.user.id && m.role !== 'owner'
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

    ${manages
      ? html`
          ${sectionHeading('Add a member', 'Anyone you add sees and can change everything this team owns.')}
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
            ${isOwner
              ? field({
                  id: 'invite-role',
                  label: 'Role',
                  error: result.fieldErrors?.role,
                  control: html`
                    <div class=${nativeSelectWrapperClass()}>
                      <select id="invite-role" name="role" data-size="sm" class=${nativeSelectClass()}>
                        <option value="member">Member</option>
                        <option value="admin">Admin</option>
                      </select>
                      <svg class=${nativeSelectIconClass()} viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m6 9 6 6 6-6" /></svg>
                    </div>
                  `,
                })
              : ''}
            <button type="submit" class=${buttonClass()}>Add</button>
          </form>
          ${footnote(
            'An admin adds and removes people and mints tokens. Only the owner can rename the team, hand it over, delete it or change its plan.',
          )}
        `
      : ''}

    ${planSection(billing, ctx.org.personal, isOwner, result)}
    ${buildersSection(builders, manages)}
    ${isOwner && !ctx.org.personal ? renameSection(ctx.org.name, result) : ''}

    <div class="mt-10 pt-8 border-t border-border">
      ${sectionHeading('Account', 'Switch teams or sign out. These work with scripting off, which the header menu does not.')}
      ${lede('The same actions the account menu in the header holds. They live here too so they work with scripting off.')}
      <div class="flex flex-wrap items-end gap-6">
        ${orgs.length > 1
          ? html`
              <form action=${switchOrg} class=${formRowClass()}>
                <input type="hidden" name="back" value="/org">
                ${field({
                  id: 'org-switch',
                  label: 'Team',
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
        <a href="/org/new" class=${buttonClass({ variant: 'outline', size: 'sm' })}>Create a team</a>
        <form method="POST" action="/api/auth/signout">
          <button type="submit" class=${buttonClass({ variant: 'outline', size: 'sm' })}>Sign out</button>
        </form>
      </div>
    </div>

    ${ctx.org.personal ? '' : dangerSection(ctx.org.name, isOwner, others, result)}
  `;
}

/** The current plan, what it buys, and the account behind it. */
function planSection(
  billing: BillingView | null,
  personal: boolean,
  isOwner: boolean,
  result: { fieldErrors?: Record<string, string> },
) {
  if (!billing) return '';
  const current = billing.plan;
  return html`
    <div class="mt-10 pt-8 border-t border-border">
      ${sectionHeading('Plan', 'What this team is allowed to run at once. Changing it takes effect immediately.')}
      <div class="grid gap-3 sm:grid-cols-2">
        ${PLAN_IDS.map((id) => planCard(PLANS[id], current.id === id, personal, isOwner))}
      </div>
      ${result.fieldErrors?.plan ? html`<p class="m-0 mt-2 text-meta text-destructive">${result.fieldErrors.plan}</p>` : ''}

      <div class=${cn(cardClass(), cardBody(), 'mt-4 items-start gap-1')}>
        <p class="m-0 text-body font-medium">Billing</p>
        <p class="m-0 text-meta text-muted-foreground">${billing.status.detail}</p>
        ${billing.since
          ? html`<p class="m-0 text-meta text-muted-foreground">
              Last changed <relative-time datetime=${billing.since.toISOString()}></relative-time>.
            </p>`
          : ''}
      </div>
      ${footnote(
        personal
          ? 'A personal team is always on the free plan. Create a team to choose another.'
          : 'No card is on file and none is asked for: this deployment has no payment provider configured.',
      )}
    </div>
  `;
}

function planCard(plan: Plan, current: boolean, personal: boolean, isOwner: boolean) {
  const limits: [string, string][] = [
    [NOUN.Instances, String(plan.quota.instances)],
    ['vCPUs', String(plan.quota.vcpus)],
    ['Memory', `${Math.round(plan.quota.memMib / 1024)} GiB`],
    [NOUN.Storage, `${plan.quota.storageGib} GiB`],
    ['Builds at once', String(plan.quota.builds)],
  ];
  return html`
    <div class=${cn(cardClass(), cardBody(), 'items-start gap-2', current ? 'border-primary' : '')}>
      <p class="m-0 flex items-center gap-2 text-body font-medium">
        ${plan.label}
        ${current ? html`<span class=${badgeClass()}>Current</span>` : ''}
      </p>
      <p class="m-0 text-meta text-muted-foreground">${plan.blurb}</p>
      <p class="m-0 text-meta">${plan.usdPerMonth === 0 ? 'Free' : `$${plan.usdPerMonth} a month`}</p>
      <dl class="m-0 grid grid-cols-2 gap-x-4 gap-y-1 text-meta w-full">
        ${limits.map(
          ([label, value]) => html`
            <dt class="m-0 text-muted-foreground">${label}</dt>
            <dd class="m-0 text-right">${value}</dd>
          `,
        )}
      </dl>
      ${current || personal || !isOwner
        ? ''
        : html`
            <form action=${activatePlan} class="mt-1">
              <input type="hidden" name="plan" value=${plan.id}>
              <button type="submit" class=${buttonClass({ variant: 'outline', size: 'sm' })}>
                Switch to ${plan.label}
              </button>
            </form>
          `}
    </div>
  `;
}

/**
 * The builders this team has, one per place that has built for it.
 *
 * Deliberately not in the main list: nobody created these and there is nothing
 * to open on one. They are here because when a build is slow or wrong, the
 * question is which one is doing it and whether its cache should be thrown
 * away, and Reset is the answer to the second half.
 */
function buildersSection(builders: Builder[], manages: boolean) {
  return html`
    <div class="mt-10 pt-8 border-t border-border">
      ${sectionHeading(
        'Builders',
        'Where this team’s images are built. Resetting one throws away its layer cache, so the next build is slower and starts clean.',
      )}
      ${builders.length === 0
        ? sectionEmpty('Nothing has been built for this team yet.', {
            text: `Deploy an ${NOUN.App.toLowerCase()} to get one`,
            href: '/services/new',
          })
        : dataTable<Builder>({
            caption: 'Builders for this team',
            rows: builders,
            columns: [
              { header: 'Where it runs', cell: (b) => html`<span class="font-mono text-meta">${b.host_id}</span>` },
              {
                header: 'Size',
                cellClass: 'text-muted-foreground',
                cell: (b) => `${b.vcpus ?? 0} vCPU, ${Math.round((b.mem_mib ?? 0) / 1024)} GiB`,
              },
              { header: 'Status', cell: (b) => stateLabel(b.state).word },
              {
                // `last_activity`, which is the field the engine actually
                // stamps; there is no `last_used_at` on a machine and a column
                // reading one would have said "Never" forever.
                header: 'Last used',
                cellClass: 'text-muted-foreground',
                cell: (b) => when(b.last_activity) || 'Never',
              },
              {
                header: 'Actions',
                headerHidden: true,
                align: 'right',
                cell: (b) =>
                  manages
                    ? html`
                        <form action=${resetBuilder}>
                          <input type="hidden" name="host" value=${b.host_id}>
                          <button type="submit" class=${buttonClass({ variant: 'outline', size: 'sm' })}>Reset</button>
                        </form>
                      `
                    : '',
              },
            ],
          })}
    </div>
  `;
}

function renameSection(name: string, result: { fieldErrors?: Record<string, string> }) {
  return html`
    <div class="mt-10 pt-8 border-t border-border">
      ${sectionHeading('Name', 'The name and the address move together, so every page keeps calling it the same thing.')}
      <form action=${renameOrg} class=${formRowClass()}>
        ${field({
          id: 'org-name',
          label: 'Name',
          error: result.fieldErrors?.name,
          control: html`<input
            id="org-name"
            name="name"
            value=${name}
            required
            maxlength="60"
            aria-invalid=${result.fieldErrors?.name ? 'true' : 'false'}
            class=${inputClass()}
          >`,
        })}
        <button type="submit" class=${buttonClass({ variant: 'outline' })}>Rename</button>
      </form>
    </div>
  `;
}

/**
 * Hand over, leave, delete.
 *
 * One rule above them, plain forms inside them, and the delete asks for the
 * name to be typed. Nothing here is reachable by a stray click and nothing
 * here needs JavaScript to work.
 */
function dangerSection(
  name: string,
  isOwner: boolean,
  others: MemberRow[],
  result: { fieldErrors?: Record<string, string> },
) {
  return html`
    <div class="mt-10 pt-8 border-t border-destructive/40">
      ${sectionHeading('Ending things', 'Each of these is hard to undo. Read the sentence under it first.')}
      <div class="grid gap-6">
        ${isOwner && others.length > 0
          ? html`
              <div>
                <form action=${transferOwnership} class=${formRowClass()}>
                  ${field({
                    id: 'transfer-to',
                    label: 'Hand the team over to',
                    control: html`
                      <div class=${nativeSelectWrapperClass()}>
                        <select id="transfer-to" name="user" data-size="sm" class=${nativeSelectClass()}>
                          ${others.map((m) => html`<option value=${String(m.userId)}>${m.login}</option>`)}
                        </select>
                        <svg class=${nativeSelectIconClass()} viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m6 9 6 6 6-6" /></svg>
                      </div>
                    `,
                  })}
                  <button type="submit" class=${buttonClass({ variant: 'outline' })}>Hand over</button>
                </form>
                ${footnote('They become the owner and you become an admin. You can leave afterwards.')}
              </div>
            `
          : ''}

        <div>
          <form action=${leaveOrg}>
            <button type="submit" class=${buttonClass({ variant: 'outline' })}>Leave this team</button>
          </form>
          ${footnote(
            'You lose access to everything it owns. The last owner cannot leave: hand the team over first.',
          )}
        </div>

        ${isOwner
          ? html`
              <div>
                <form action=${deleteOrg} class=${formRowClass()}>
                  ${field({
                    id: 'delete-confirm',
                    label: html`Type <strong>${name}</strong> to delete this team`,
                    error: result.fieldErrors?.confirm,
                    control: html`<input
                      id="delete-confirm"
                      name="confirm"
                      autocomplete="off"
                      placeholder=${name}
                      aria-invalid=${result.fieldErrors?.confirm ? 'true' : 'false'}
                      class=${inputClass()}
                    >`,
                  })}
                  <button type="submit" class=${buttonClass({ variant: 'destructive' })}>Delete team</button>
                </form>
                ${footnote(
                  'It has to be empty first: remove everything it runs, and every token it minted stops working the moment it goes.',
                )}
              </div>
            `
          : ''}
      </div>
    </div>
  `;
}
