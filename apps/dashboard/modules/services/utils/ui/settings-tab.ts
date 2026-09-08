/**
 * The Settings tab: how many copies run, which repository deploys it, and
 * which domains point at it. One sentence under every setting, because a
 * person who has never read this repo should not need a glossary to change
 * one.
 */

import { html } from '@webjsdev/core';
import type { TemplateResult } from '@webjsdev/core';
import type { DomainResponse } from '@pilots/sdk';
import type { TabProps } from '#modules/services/utils/tabs.ts';
import { patchService } from '#modules/services/actions/patch-service.server.ts';
import { connectRepo } from '#modules/github/actions/connect-repo.server.ts';
import { disconnectRepo } from '#modules/github/actions/disconnect-repo.server.ts';
import { addDomain } from '#modules/domains/actions/add-domain.server.ts';
import { deleteDomain } from '#modules/domains/actions/delete-domain.server.ts';
import { badgeClass } from '#components/ui/badge.ts';
import { buttonClass } from '#components/ui/button.ts';
import { checkboxClass } from '#components/ui/checkbox.ts';
import { inputClass } from '#components/ui/input.ts';
import { labelClass } from '#components/ui/label.ts';
import { dataTable, field, formRowClass, sectionGap, sectionHeading } from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';
import { NOUN } from '#lib/vocabulary.ts';

export function settingsTab({ detail, back, errors }: TabProps): TemplateResult {
  const { service, repo, domains, github } = detail;

  return html`
    <div class=${sectionGap()}>
      <section>
        ${sectionHeading(NOUN.Instances, 'How many copies run. A service that mounts storage runs exactly one.')}
        <form action=${patchService} class=${formRowClass()}>
          <input type="hidden" name="service" value=${service.id}>
          <input type="hidden" name="back" value=${back}>
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

      <section>
        ${sectionHeading(
          'Repository',
          "Every push to the branch deploys it. The pilots app must be installed on the repository's owner.",
        )}
        ${repo
          ? html`
              <dl class="m-0 mb-3 grid grid-cols-[max-content_1fr] gap-x-6 gap-y-1 text-body">
                <dt class="text-muted-foreground">Repository</dt>
                <dd class="m-0"><a href=${`https://github.com/${repo.repo}`} rel="noopener">${repo.repo}</a></dd>
                <dt class="text-muted-foreground">Branch</dt>
                <dd class="m-0 font-mono">${repo.branch}</dd>
                <dt class="text-muted-foreground">Deploy on push</dt>
                <dd class="m-0">
                  <span class=${badgeClass({ variant: repo.autodeploy ? 'secondary' : 'outline' })}
                    >${repo.autodeploy ? 'on' : 'off'}</span
                  >
                </dd>
                <dt class="text-muted-foreground">GitHub app</dt>
                <dd class="m-0">
                  ${!github.configured
                    ? 'Not configured on this fleet'
                    : repo.installationId
                      ? `Installed (#${repo.installationId})`
                      : html`Not installed on <strong>${repo.repo.split('/')[0]}</strong>, so pushes are ignored.
                          <a href=${github.installUrl} rel="noopener">Install it</a>`}
                </dd>
              </dl>
              <form action=${disconnectRepo}>
                <input type="hidden" name="service" value=${service.id}>
                <input type="hidden" name="back" value=${back}>
                <button type="submit" class=${buttonClass({ variant: 'outline' })}>Disconnect</button>
              </form>
            `
          : html`
              <form action=${connectRepo} class=${formRowClass()}>
                <input type="hidden" name="service" value=${service.id}>
                <input type="hidden" name="back" value=${back}>
                ${field({
                  id: 'repo',
                  label: 'Repository',
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
                  Deploy on push
                </label>
                <button type="submit" class=${buttonClass()}>Connect</button>
              </form>
            `}
      </section>

      <section>
        ${sectionHeading(
          'Domains',
          'A custom domain points at this service with a CNAME. The certificate is issued once the record resolves.',
        )}
        ${domains.length === 0
          ? html`<p class="m-0 mb-3 text-meta text-muted-foreground">
              No custom domain yet. The service answers on its own URL until one is added.
            </p>`
          : html`<div class="mb-4">
              ${dataTable<DomainResponse>({
                caption: 'Custom domains pointing at this service',
                rows: domains,
                columns: [
                  { header: 'Hostname', cell: (d) => d.hostname },
                  {
                    header: 'Certificate',
                    cell: (d) =>
                      html`<span class=${badgeClass({ variant: d.verified ? 'secondary' : 'outline' })}
                        >${d.verified ? 'Issued' : 'Waiting for the record'}</span
                      >`,
                  },
                  { header: 'CNAME target', cellClass: 'font-mono text-muted-foreground', cell: (d) => d.cname_target },
                  {
                    header: 'Actions',
                    headerHidden: true,
                    align: 'right',
                    cell: (d) => html`
                      <form action=${deleteDomain}>
                        <input type="hidden" name="hostname" value=${d.hostname}>
                        <input type="hidden" name="back" value=${back}>
                        <button type="submit" class=${buttonClass({ variant: 'outline', size: 'sm' })}>Remove</button>
                      </form>
                    `,
                  },
                ],
              })}
            </div>`}
        <form action=${addDomain} class=${formRowClass()}>
          <input type="hidden" name="service" value=${service.id}>
          <input type="hidden" name="back" value=${back}>
          ${field({
            id: 'hostname',
            label: 'Hostname',
            error: errors.fieldErrors?.hostname,
            control: html`<input
              id="hostname"
              name="hostname"
              placeholder="app.example.com"
              required
              aria-invalid=${errors.fieldErrors?.hostname ? 'true' : 'false'}
              class=${inputClass()}
            >`,
          })}
          <button type="submit" class=${buttonClass({ variant: 'outline' })}>Add domain</button>
        </form>
      </section>
    </div>
  `;
}
