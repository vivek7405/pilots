/**
 * The Deployments tab: what is live, what came before it, and how to deploy
 * something else.
 *
 * Only the newest healthy deployment BEFORE the current one gets a roll-back
 * button. One that never passed its health gate was never serving traffic,
 * so rolling "back" to it would be a deploy of something known broken.
 */

import { html } from '@webjsdev/core';
import type { TemplateResult } from '@webjsdev/core';
import type { Machine, Release } from '@pilots/sdk';
import type { TabProps } from '#modules/services/utils/tabs.ts';
import { deployService } from '#modules/services/actions/deploy-service.server.ts';
import { rollbackService } from '#modules/services/actions/rollback-service.server.ts';
import { statusDot } from '#modules/machines/utils/ui/state.ts';
import { badgeClass } from '#components/ui/badge.ts';
import { buttonClass } from '#components/ui/button.ts';
import { checkboxClass } from '#components/ui/checkbox.ts';
import { inputClass } from '#components/ui/input.ts';
import { labelClass } from '#components/ui/label.ts';
import { cardClass } from '#components/ui/card.ts';
import { cardBody, dataTable, field, formRowClass, sectionEmpty, sectionGap, sectionHeading } from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';
import { NOUN } from '#lib/vocabulary.ts';
import '#components/copy-button.ts';
import '#modules/services/components/build-log.ts';
import '#components/relative-time.ts';

export function deploymentsTab({ detail, back, errors, build }: TabProps): TemplateResult {
  const { service, releases, replicas, previews, repo } = detail;
  const builds = detail.builds ?? [];
  const following = build ? builds.find((b) => b.jobId === build) : undefined;
  const current = releases.find((r) => r.id === service.release_id);
  const rollbackTarget = releases.filter((r) => r.healthy && r.id !== service.release_id)[0];
  const running = replicas.filter((m) => m.state === 'running').length;
  const answering = replicas.filter((m) => m.state === 'running' || m.state === 'suspended').length;

  return html`
    <div class=${sectionGap()}>
      ${following
        ? html`<section>
            ${sectionHeading(
              'Building',
              html`The image is being built from <span class="font-mono">${following.repo}@${following.ref}</span>. When it
              succeeds it is deployed and this page moves to the new deployment.`,
            )}
            <build-log build-id=${following.jobId} service-id=${service.id} autodeploy></build-log>
            <p class="m-0 mt-2 text-meta text-muted-foreground">
              With scripting off, the last line of the raw log carries the image id; the Deploy form below takes it.
            </p>
          </section>`
        : builds.length > 0
          ? html`<section>
              ${sectionHeading('Builds', 'Images built from this repository, newest first.')}
              <ul class="m-0 grid list-none gap-1 p-0 text-meta">
                ${builds.map(
                  (b) => html`<li class="flex flex-wrap items-center gap-x-3">
                    <span class="font-mono">${b.repo}@${b.ref}</span>
                    <relative-time datetime=${String(Math.floor(b.createdAt.getTime() / 1000))}></relative-time>
                    <a href=${`${back}&build=${encodeURIComponent(b.jobId)}`}>Follow</a>
                    <a href=${`/api/builds/${encodeURIComponent(b.jobId)}/logs`}>View log</a>
                  </li>`,
                )}
              </ul>
            </section>`
          : ''}
      <p class="m-0 flex flex-wrap items-center gap-x-3 text-meta text-muted-foreground">
        <span
          >${answering}/${service.replicas} ${service.replicas === 1 ? 'instance' : 'instances'} online${running <
          answering
            ? ', some asleep'
            : ''}</span
        >
      </p>

      <section>
        ${current
          ? html`<div class=${cn(cardClass(), 'gap-0 py-0')} data-current-deployment>
              <div class=${cn(cardBody(), 'grid gap-3')}>
                <div class="flex flex-wrap items-center gap-2">
                  <span class=${badgeClass({ variant: current.healthy ? 'default' : 'destructive' })}
                    >${current.healthy ? 'Live' : 'Not serving'}</span
                  >
                  <span class="flex items-center gap-1 font-mono text-body">
                    ${current.id}
                    <copy-button value=${current.id} label="deployment id"></copy-button>
                  </span>
                  <span class="text-meta text-muted-foreground">
                    deployed <relative-time datetime=${String(current.created_at)}></relative-time>${repo
                      ? ' via GitHub'
                      : ''}
                  </span>
                </div>
                ${current.rootfs_build_id
                  ? html`<p class="m-0 text-meta text-muted-foreground">
                      ${NOUN.Image} <span class="font-mono">${current.rootfs_build_id}</span>
                    </p>`
                  : ''}
                ${replicas.length > 0
                  ? html`<ul class="m-0 grid list-none gap-1 p-0 text-meta">
                      ${replicas.map(
                        (m) => html`<li class="flex items-center gap-3">
                          <a href=${`/machines/${m.id}`} class="text-foreground">${m.name || m.id}</a>
                          ${statusDot(m.state)}
                        </li>`,
                      )}
                    </ul>`
                  : html`<p class="m-0 text-meta text-muted-foreground">No instances yet.</p>`}
              </div>
            </div>`
          : sectionEmpty('Nothing is deployed yet', { text: 'Deploy an image below', href: '#deploy' })}
      </section>

      <section>
        ${sectionHeading(
          'History',
          'Every deployment, newest first. Only the newest healthy one before the current can be rolled back to.',
        )}
        ${releases.length === 0
          ? html`<p class="m-0 text-meta text-muted-foreground">No deployments recorded yet.</p>`
          : dataTable<Release>({
              caption: 'Deployments of this service, newest first',
              rows: releases,
              columns: [
                {
                  header: NOUN.Deployment,
                  cellClass: 'font-mono',
                  cell: (r) => html`
                    ${r.id}${r.id === service.release_id
                      ? html` <span class=${cn(badgeClass({ variant: 'secondary' }), 'ml-1 font-sans')}>Live</span>`
                      : ''}
                  `,
                },
                {
                  header: 'Health',
                  cell: (r) =>
                    html`<span class=${badgeClass({ variant: r.healthy ? 'secondary' : 'outline' })}
                      >${r.healthy ? 'Healthy' : 'Never passed'}</span
                    >`,
                },
                {
                  header: 'Deployed',
                  cell: (r) => html`<relative-time datetime=${String(r.created_at)}></relative-time>`,
                },
                {
                  header: NOUN.Image,
                  cellClass: 'font-mono text-muted-foreground',
                  cell: (r) => r.rootfs_build_id ?? '',
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
                            <input type="hidden" name="back" value=${back}>
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

      ${previews.length > 0
        ? html`<section>
            ${sectionHeading(
              'Pull request previews',
              'Each open pull request against the connected branch runs on its own URL until it is closed.',
            )}
            ${dataTable<Machine>({
              caption: 'Open pull request previews',
              rows: previews,
              columns: [
                {
                  header: 'Preview',
                  cell: (m) => html`<a href=${`/machines/${m.id}`} class="text-foreground">${m.name}</a>`,
                },
                { header: 'Status', cell: (m) => statusDot(m.state) },
                { header: 'URL', cell: (m) => (m.url ? html`<a href=${m.url} rel="noopener">${m.url}</a>` : '') },
              ],
            })}
          </section>`
        : ''}

      <section id="deploy">
        ${sectionHeading(
          'Deploy an image',
          'Deploy an image built by pilot deploy or by a push. Tick the box to keep one instance awake.',
        )}
        <form action=${deployService} class="grid gap-3">
          <input type="hidden" name="service" value=${service.id}>
          <input type="hidden" name="back" value=${back}>
          <div class=${formRowClass()}>
            ${field({
              id: 'release',
              label: NOUN.Deployment,
              error: errors.fieldErrors?.release,
              control: html`<input id="release" name="release" placeholder="rel_..." class=${cn(inputClass(), 'font-mono')}>`,
            })}
            ${field({
              id: 'build',
              label: `or ${NOUN.Image}`,
              error: errors.fieldErrors?.build,
              control: html`<input id="build" name="build" placeholder="bld_..." class=${cn(inputClass(), 'font-mono')}>`,
            })}
            <button type="submit" class=${buttonClass()}>Deploy</button>
          </div>
          <label class=${labelClass()} for="keep_awake">
            <input id="keep_awake" name="keep_awake" type="checkbox" data-slot="checkbox" class=${checkboxClass()}>
            Keep one instance awake
          </label>
        </form>
      </section>
    </div>
  `;
}
