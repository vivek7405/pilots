/**
 * The health pills, beside a heading.
 *
 * They render on the SERVICE page and on every machine page belonging to a
 * failing service, because a reader who navigated straight to one replica
 * should not have to go up a level to learn the service is down.
 */

import { html } from '@webjsdev/core';
import type { TemplateResult } from '@webjsdev/core';
import { badgeClass } from '#components/ui/badge.ts';
import type { ServiceHealth } from '#modules/services/utils/health.ts';

const LABEL: Record<string, string> = {
  failing: 'Replicas failing health checks',
  suspended: 'Suspended',
};

export function healthPills(health: ServiceHealth): TemplateResult | string {
  if (health.pills.length === 0) return '';
  return html`<span class="flex flex-wrap items-center gap-2">
    ${health.pills.map(
      (pill) =>
        html`<span class=${badgeClass({ variant: pill === 'failing' ? 'destructive' : 'secondary' })}
          >${LABEL[pill]}</span
        >`,
    )}
  </span>`;
}
