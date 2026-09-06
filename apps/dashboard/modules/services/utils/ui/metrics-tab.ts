/**
 * The Metrics tab: each instance of the service, what it is allotted, and how
 * it last came up (`rw-29-metrics-replicas.png`).
 *
 * Only what is measured is shown. pilots meters instance-seconds for billing
 * and does not yet keep CPU or memory over time per service, so the two chart
 * cards say so in their own body rather than drawing an empty grid, and there
 * is no range or pause toolbar for data that does not exist.
 */
import { html } from '@webjsdev/core';
import type { TemplateResult } from '@webjsdev/core';
import type { TabProps } from '#modules/services/utils/tabs.ts';
import { statusDot } from '#modules/machines/utils/ui/state.ts';
import { startLabel } from '#lib/vocabulary.ts';
import { cardClass } from '#components/ui/card.ts';
import { cardBody, sectionEmpty, sectionGap, sectionHeading } from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';
import '#components/relative-time.ts';

export function metricsTab({ detail }: TabProps): TemplateResult {
  const { replicas } = detail;

  return html`
    <div class=${sectionGap()}>
      <section>
        ${sectionHeading('Instances', 'Each copy of this service, what it is allotted, and how it last came up.')}
        ${replicas.length === 0
          ? sectionEmpty('No instances yet', { text: 'Deploy to start one', href: '#deploy' })
          : html`<div class="grid gap-3 sm:grid-cols-2">
              ${replicas.map(
                (m) => html`
                  <div class=${cn(cardClass(), 'gap-0 py-0')}>
                    <div class=${cn(cardBody(), 'grid gap-2')}>
                      <div class="flex items-center justify-between gap-2">
                        <a href=${`/machines/${m.id}`} class="truncate font-medium text-foreground">${m.name || m.id}</a>
                        ${statusDot(m.state)}
                      </div>
                      <p class="m-0 text-meta text-muted-foreground">
                        ${startLabel(m.last_start)}${m.last_start_at
                          ? html` <relative-time datetime=${String(m.last_start_at)}></relative-time>`
                          : ''}
                      </p>
                      <p class="m-0 flex flex-wrap gap-x-3 text-meta">
                        <span>${m.vcpus ?? 1} vCPU</span>
                        <span>${((m.mem_mib ?? 512) / 1024).toFixed(1)} GB</span>
                        <a href=${`/machines/${m.id}`} class="ml-auto">Logs</a>
                      </p>
                    </div>
                  </div>
                `,
              )}
            </div>`}
      </section>

      <section class="grid gap-3 sm:grid-cols-2">
        ${['CPU over time', 'Memory over time'].map(
          (title) => html`
            <div class=${cn(cardClass(), 'gap-0 py-0')}>
              <div class=${cardBody()}>
                <h3 class="m-0 mb-3 text-body font-medium">${title}</h3>
                ${sectionEmpty('Not recorded yet', {
                  text: 'pilots meters instance-seconds for billing and does not yet keep CPU or memory over time for a service.',
                  href: '/usage',
                })}
              </div>
            </div>
          `,
        )}
      </section>
    </div>
  `;
}
