/**
 * A service's state as one sentence.
 *
 * The services list had Name, Replicas, Repo and URL: four facts, none of them
 * the one a reader opens the page for, which is whether the thing is up. The
 * engine already returns everything needed to say so.
 */

import { html } from '@webjsdev/core';
import type { TemplateResult } from '@webjsdev/core';
import { badgeClass } from '#components/ui/badge.ts';
import { cn } from '#lib/utils/cn.ts';
import type { Machine } from '#modules/machines/types.ts';
import type { HealthRelease, HealthService } from '#modules/services/utils/health.ts';
import { serviceHealth } from '#modules/services/utils/health.ts';

export interface StatusService extends HealthService {
  name: string;
  replicas?: number;
  autodeploy?: boolean;
  branch?: string;
  repo?: string;
}

export function serviceStatusLine(
  service: StatusService,
  replicas: Machine[],
  releases: HealthRelease[],
): TemplateResult {
  const health = serviceHealth(service, replicas, releases);
  const running = replicas.filter((r) => r.state === 'running').length;
  const wanted = service.replicas ?? replicas.length;
  const release = health.release;

  const count =
    health.pills.includes('failing') || running < wanted
      ? html`<span class="text-destructive">${running}/${wanted} instances online</span>`
      : html`${running}/${wanted} instances online`;

  const asleep = health.pills.includes('suspended');

  // Each phrase is nowrap so a narrow column breaks between them rather than
  // through `1 hour ago`.
  return html`<span class="flex flex-wrap items-center gap-x-2 gap-y-1 text-meta">
    <span class="whitespace-nowrap">${count}</span>
    ${asleep
      ? html`<span class=${cn(badgeClass({ variant: 'secondary' }), 'whitespace-nowrap')}
          >Sleeping · wakes on request</span
        >`
      : ''}
    ${release?.created_at
      ? html`<span class="text-muted-foreground whitespace-nowrap"
          >deployed <relative-time datetime=${String(release.created_at)}></relative-time
        ></span>`
      : ''}
    ${service.autodeploy && service.branch
      ? html`<span class="text-muted-foreground whitespace-nowrap">from ${service.branch}</span>`
      : ''}
  </span>`;
}
