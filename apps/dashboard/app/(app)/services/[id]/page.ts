/**
 * One service: releases, deploy, rollback, PR previews, the repo connection.
 *
 * Only the PREVIOUS healthy release gets a rollback button. A release that
 * never passed its health gate was never serving traffic, so rolling "back" to
 * it would be a deploy of something known broken.
 */
import { html, notFound } from '@webjsdev/core';
import type { PageProps } from '@webjsdev/core';
import type { Machine, Release } from '@pilots/sdk';
import { orUnauthorized, requireOrg } from '#modules/auth/session.server.ts';
import { getService } from '#modules/services/queries/get-service.server.ts';
import { deployService } from '#modules/services/actions/deploy-service.server.ts';
import { rollbackService } from '#modules/services/actions/rollback-service.server.ts';
import { patchService } from '#modules/services/actions/patch-service.server.ts';
import { connectRepo } from '#modules/github/actions/connect-repo.server.ts';
import { disconnectRepo } from '#modules/github/actions/disconnect-repo.server.ts';
import { githubAppConfigured } from '#modules/github/app-jwt.server.ts';
import { installUrl } from '#modules/github/installations.server.ts';
import { stateBadge } from '#modules/machines/utils/ui/state.ts';
import { badgeClass } from '#components/ui/badge.ts';
import { buttonClass } from '#components/ui/button.ts';
import { checkboxClass } from '#components/ui/checkbox.ts';
import { inputClass } from '#components/ui/input.ts';
import { labelClass } from '#components/ui/label.ts';
import { dataTable, emptyState, errorAlert, field, formRowClass, pageHeading, sectionHeading } from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';

export async function generateMetadata({ params }: PageProps) {
  return { title: `Service ${params.id}` };
}

export default async function ServicePage({ params, actionData }: PageProps) {
  const ctx = (await requireOrg())!;
  const detail = orUnauthorized(await getService({ id: params.id }));
  if (!detail) throw notFound();
  const { service, releases, previews, repo } = detail;

  const errors = (actionData as { fieldErrors?: Record<string, string>; error?: string } | undefined) ?? {};
  // The current release is not a rollback target; the newest healthy one
  // BEFORE it is.
  const rollbackTarget = releases.filter((r) => r.healthy && r.id !== service.release_id)[0];
  const appConfigured = githubAppConfigured();

  return html`
    ${pageHeading(service.name)}
    <p class="text-muted-foreground mt-1 mb-6">
      ${service.url ? html`<a href=${service.url} rel="noopener">${service.url}</a>` : 'No URL yet'}
    </p>

    ${errors.error ? errorAlert(errors.error) : ''}

    <section class="mb-8">
      ${sectionHeading('Scale')}
      <form action=${patchService} class=${formRowClass()}>
        <input type="hidden" name="service" value=${service.id}>
        ${field({
          id: 'replicas',
          label: 'Replicas',
          error: errors.fieldErrors?.replicas,
          control: html`<input
            id="replicas"
            name="replicas"
            type="number"
            min="0"
            max="100"
            value=${String(service.replicas)}
            aria-invalid=${errors.fieldErrors?.replicas ? 'true' : 'false'}
            class=${cn(inputClass(), 'w-24 tabular-nums')}
          >`,
        })}
        <button type="submit" class=${buttonClass()}>Save</button>
      </form>
    </section>

    <section class="mb-8">
      ${sectionHeading('Releases')}
      ${releases.length === 0
        ? emptyState("No releases recorded. The engine's release history route is not serving yet.")
        : dataTable<Release>({
            caption: 'Releases of this service, newest first',
            rows: releases,
            columns: [
              {
                header: 'Release',
                cellClass: 'font-mono',
                cell: (r) => html`
                  ${r.id}${r.id === service.release_id
                    ? html` <span class=${cn(badgeClass({ variant: 'secondary' }), 'ml-1')}>current</span>`
                    : ''}
                `,
              },
              {
                header: 'Health',
                cell: (r) =>
                  html`<span class=${badgeClass({ variant: r.healthy ? 'secondary' : 'outline' })}
                    >${r.healthy ? 'healthy' : 'never passed'}</span
                  >`,
              },
              {
                header: 'Build',
                cellClass: 'font-mono text-muted-foreground',
                cell: (r) => r.rootfs_build_id ?? '-',
              },
              {
                header: 'Actions',
                headerHidden: true,
                align: 'right',
                cell: (r) =>
                  rollbackTarget && r.id === rollbackTarget.id
                    ? html`
                        <form action=${rollbackService}>
                          <input type="hidden" name="service" value=${service.id}>
                          <button type="submit" class=${buttonClass({ variant: 'outline', size: 'sm' })}>
                            Roll back to this
                          </button>
                        </form>
                      `
                    : '',
              },
            ],
          })}
    </section>

    <section class="mb-8">
      ${sectionHeading('Deploy')}
      <form action=${deployService} class=${formRowClass()}>
        <input type="hidden" name="service" value=${service.id}>
        ${field({
          id: 'release',
          label: 'Release',
          control: html`<input id="release" name="release" placeholder="rel_..." class=${cn(inputClass(), 'font-mono')}>`,
        })}
        ${field({
          id: 'build',
          label: 'or Build',
          control: html`<input id="build" name="build" placeholder="bld_..." class=${cn(inputClass(), 'font-mono')}>`,
        })}
        <button type="submit" class=${buttonClass()}>Deploy</button>
      </form>
    </section>

    <section class="mb-8">
      ${sectionHeading('Repository')}
      ${repo
        ? html`
            <dl class="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-1 text-sm mb-3">
              <dt class="text-muted-foreground">Repo</dt>
              <dd class="m-0"><a href=${`https://github.com/${repo.repo}`} rel="noopener">${repo.repo}</a></dd>
              <dt class="text-muted-foreground">Branch</dt>
              <dd class="m-0 font-mono">${repo.branch}</dd>
              <dt class="text-muted-foreground">Autodeploy</dt>
              <dd class="m-0">
                <span class=${badgeClass({ variant: repo.autodeploy ? 'secondary' : 'outline' })}
                  >${repo.autodeploy ? 'on' : 'off'}</span
                >
              </dd>
              <dt class="text-muted-foreground">App</dt>
              <dd class="m-0">
                ${!appConfigured
                  ? 'App not configured on this fleet'
                  : repo.installationId
                    ? `installed (#${repo.installationId})`
                    : html`not installed on <strong>${repo.repo.split('/')[0]}</strong>, so pushes are ignored -
                        <a href=${installUrl()} rel="noopener">install it</a>`}
              </dd>
            </dl>
            <form action=${disconnectRepo}>
              <input type="hidden" name="service" value=${service.id}>
              <button type="submit" class=${buttonClass({ variant: 'outline' })}>Disconnect</button>
            </form>
          `
        : html`
            <form action=${connectRepo} class=${formRowClass()}>
              <input type="hidden" name="service" value=${service.id}>
              ${field({
                id: 'repo',
                label: 'Repo',
                error: errors.fieldErrors?.repo,
                control: html`<input
                  id="repo"
                  name="repo"
                  placeholder="owner/name"
                  required
                  aria-invalid=${errors.fieldErrors?.repo ? 'true' : 'false'}
                  class=${inputClass()}
                >`,
              })}
              ${field({
                id: 'branch',
                label: 'Branch',
                control: html`<input id="branch" name="branch" value="main" class=${cn(inputClass(), 'font-mono')}>`,
              })}
              <label class=${cn(labelClass(), 'h-9')} for="autodeploy">
                <input id="autodeploy" name="autodeploy" type="checkbox" data-slot="checkbox" checked class=${checkboxClass()}>
                Autodeploy
              </label>
              <button type="submit" class=${buttonClass()}>Connect</button>
            </form>
          `}
    </section>

    <section>
      ${sectionHeading('Pull-request previews')}
      ${previews.length === 0
        ? emptyState('None open.')
        : dataTable<Machine>({
            caption: 'Machines serving open pull-request previews',
            rows: previews,
            columns: [
              {
                header: 'Preview',
                cell: (m) => html`<a href=${`/machines/${m.id}`} class="text-foreground">${m.name}</a>`,
              },
              { header: 'State', cell: (m) => stateBadge(m.state) },
              { header: 'URL', cell: (m) => (m.url ? html`<a href=${m.url} rel="noopener">${m.url}</a>` : '') },
            ],
          })}
    </section>
  `;
}
