/**
 * The Metrics tab: each instance of the service, what it is allotted, and how
 * it last came up (`rw-29-metrics-replicas.png`).
 *
 * Only what is measured is shown. pilots meters instance-seconds for billing
 * and does not yet keep CPU or memory over time per service, so the two chart
 * cards say so in their own body rather than drawing an empty grid, and there
 * is no range or pause toolbar for data that does not exist.
 *
 * This list is the one place that shows EVERY machine attached to the service,
 * including one left behind on a superseded release. The counts elsewhere
 * narrow to the current release the way the engine does, so without this a
 * leftover would hold a URL and burn quota with nothing on any page naming it.
 * It is marked rather than mixed in: the engine's autoscaler cannot see it, so
 * it will not be retired on its own.
 */
import { html } from '@webjsdev/core';
import type { TemplateResult } from '@webjsdev/core';
import type { TabProps } from '#modules/services/utils/tabs.ts';
import { statusDot } from '#modules/machines/utils/ui/state.ts';
import { startLabel } from '#lib/vocabulary.ts';
import { isStaleReplica } from '#modules/services/utils/replicas.ts';
import '#modules/machines/components/live-machine-state.ts';
import { badgeClass } from '#components/ui/badge.ts';
import { cardClass } from '#components/ui/card.ts';
import { cardBody, sectionEmpty, sectionGap, sectionHeading } from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';
import '#components/relative-time.ts';

export function metricsTab({ detail }: TabProps): TemplateResult {
  const { replicas, service } = detail;

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
                        <live-machine-state machine-id=${m.id} mode="dot">${statusDot(m.state)}</live-machine-state>
                      </div>
                      ${isStaleReplica(m, service)
                        ? html`<p class="m-0">
                            <span class=${badgeClass({ variant: 'outline' })} title="Left behind by a deploy: this service points at a newer release, and the engine's autoscaler only manages instances on that one."
                              >On an older release</span
                            >
                          </p>`
                        : ''}
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
