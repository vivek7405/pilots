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
import { statusDot } from '#modules/machines/utils/ui/state.ts';
import { statusLine } from '#modules/machines/utils/ui/status-line.ts';
import { serviceHealth } from '#modules/services/utils/health.ts';
import { healthPills } from '#modules/services/utils/ui/health-pills.ts';
import { doctorCard } from '#modules/services/utils/ui/doctor-card.ts';
import { badgeClass } from '#components/ui/badge.ts';
import { buttonClass } from '#components/ui/button.ts';
import { checkboxClass } from '#components/ui/checkbox.ts';
import { inputClass } from '#components/ui/input.ts';
import { labelClass } from '#components/ui/label.ts';
import { dataTable, emptyState, errorAlert, field, formRowClass, pageHeading, sectionHeading } from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';
import { NOUN, SLEEP_SENTENCE } from '#lib/vocabulary.ts';
import type { Machine as BrowserMachine } from '#modules/machines/types.ts';
import '#components/copy-button.ts';

export async function generateMetadata({ params }: PageProps) {
  return { title: `Service ${params.id}` };
}

export default async function ServicePage({ params, actionData }: PageProps) {
  const ctx = (await requireOrg())!;
  const detail = orUnauthorized(await getService({ id: params.id }));
  if (!detail) throw notFound();
  const { service, releases, previews, repo, replicas, hosts } = detail;
  const health = serviceHealth(service, replicas as BrowserMachine[], releases);

  const errors = (actionData as { fieldErrors?: Record<string, string>; error?: string } | undefined) ?? {};
  // The current release is not a rollback target; the newest healthy one
  // BEFORE it is.
  const rollbackTarget = releases.filter((r) => r.healthy && r.id !== service.release_id)[0];
  const appConfigured = githubAppConfigured();

  return html`
    <div class="flex flex-wrap items-center gap-3">
      ${pageHeading(service.name)} ${healthPills(health)}
    </div>
    <div class="mt-2 mb-6 flex flex-wrap items-center gap-x-6 gap-y-2 text-meta text-muted-foreground">
      ${service.url
        ? html`<span class="flex items-center gap-1">
            <a href=${service.url} rel="noopener">${service.url}</a>
            <copy-button value=${service.url} label="URL"></copy-button>
          </span>`
        : 'No URL yet'}
      <span>${replicas.filter((m) => m.state === 'running').length}/${service.replicas} ${NOUN.Instances.toLowerCase()} online</span>
      ${service.app ? html`<span>app ${service.app}</span>` : ''}
      ${service.release_id
        ? html`<span class="flex items-center gap-1"
            >release <span class="font-mono">${service.release_id}</span>
            <copy-button value=${service.release_id} label="release id"></copy-button>
          </span>`
        : ''}
      ${repo
        ? html`<span class=${badgeClass({ variant: repo.autodeploy ? 'secondary' : 'outline' })}
            >Autodeploy ${repo.autodeploy ? 'on' : 'off'}</span
          >`
        : ''}
    </div>

    ${errors.error ? errorAlert(errors.error) : ''}
    ${doctorCard({
      health,
      replicas: replicas as BrowserMachine[],
      serviceName: service.name,
      // The deploy action's own words when the failure is this fresh, rather
      // than a paraphrase of them.
      ...(errors.error ? { symptom: errors.error } : {}),
    })}

    ${replicas.length > 0
      ? html`<section class="mb-8">
          ${sectionHeading(NOUN.Instances, 'Every copy of this service that is running right now.')}
          ${dataTable<Machine>({
            caption: 'Machines running this service',
            rows: replicas,
            columns: [
              {
                header: 'Name',
                cell: (m) => html`<a href=${`/machines/${m.id}`} class="text-foreground">${m.name || m.id}</a>`,
              },
              { header: 'Status', cell: (m) => statusLine(m as BrowserMachine, hosts) },
              { header: 'Host', cellClass: 'font-mono text-muted-foreground', cell: (m) => m.host_id ?? '' },
              {
                header: 'Release',
                cellClass: 'font-mono text-muted-foreground',
                cell: (m) => m.release_id ?? '',
              },
            ],
          })}
        </section>`
      : ''}

    <section class="mb-8">
      ${sectionHeading(NOUN.Instances, 'How many copies run. A service that mounts storage runs exactly one.')}
      <form action=${patchService} class=${formRowClass()}>
        <input type="hidden" name="service" value=${service.id}>
        ${field({
          id: 'replicas',
          label: NOUN.Instances,
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
      ${sectionHeading(NOUN.Deployments, 'Every deployment, newest first. Only the newest healthy one before the current can be rolled back to.')}
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
      ${sectionHeading('Deploy an image', `Deploy an ${NOUN.Image.toLowerCase()} built by pilot deploy or by a push.`)}
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
      ${sectionHeading('Repository', `Every push to the branch deploys it. The pilots app must be installed on the repository's owner.`)}
      ${repo
        ? html`
            <dl class="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-1 text-meta mb-3">
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
      ${sectionHeading('Pull request previews', `A sandbox per open pull request, built from its head commit. ${SLEEP_SENTENCE}`)}
      ${previews.length === 0
        ? emptyState('None open. A pull request against the connected branch gets a sandbox of its own, with its own URL.')
        : dataTable<Machine>({
            caption: 'Machines serving open pull-request previews',
            rows: previews,
            columns: [
              {
                header: 'Preview',
                cell: (m) => html`<a href=${`/machines/${m.id}`} class="text-foreground">${m.name}</a>`,
              },
              { header: 'State', cell: (m) => statusDot(m.state) },
              { header: 'URL', cell: (m) => (m.url ? html`<a href=${m.url} rel="noopener">${m.url}</a>` : '') },
            ],
          })}
    </section>
  `;
}
