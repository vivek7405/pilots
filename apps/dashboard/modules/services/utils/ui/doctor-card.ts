/**
 * The doctor card: what is wrong, which replicas say so, and what to do next.
 *
 * It is not a status widget. Every line in it is a thing a person can act on,
 * in the order they would act on them, because the failure it exists for
 * (a release that never passes its health gate) has four ordinary causes and
 * the dashboard used to name none of them.
 *
 * The symptom line takes the deploy action's own error when there is one, so
 * the engine's words reach the reader verbatim rather than being paraphrased
 * into something vaguer.
 */

import { html } from '@webjsdev/core';
import type { TemplateResult } from '@webjsdev/core';
import { cardClass } from '#components/ui/card.ts';
import { cn } from '#lib/utils/cn.ts';
import type { Machine } from '#modules/machines/types.ts';
import type { ServiceHealth } from '#modules/services/utils/health.ts';

export function doctorCard(opts: {
  health: ServiceHealth;
  replicas: Machine[];
  serviceName: string;
  /** The error string the deploy action returned, when the failure is fresh. */
  symptom?: string;
}): TemplateResult | string {
  const { health, replicas, serviceName } = opts;
  if (!health.pills.includes('failing')) return '';

  const named = health.failing
    .map((id) => replicas.find((r) => r.id === id))
    .filter((r): r is Machine => Boolean(r));
  const first = named[0];

  const symptom =
    opts.symptom ??
    (health.release
      ? `Deployment ${health.release.id} has not passed its health check in ${health.graceSec} s.`
      : 'This service has no instance answering.');

  return html`
    <div class=${cn(cardClass(), 'border-destructive/40 mb-6')} role="group" aria-label="Diagnosis">
      <h2 class="m-0 text-heading font-medium">This service is not serving</h2>
      <p class="m-0 text-body text-destructive">${symptom}</p>

      <p class="m-0 text-meta text-muted-foreground">
        ${named.length === 1 ? 'The instance' : 'The instances'} involved:
        ${named.map(
          (replica, index) =>
            html`${index > 0 ? ', ' : ''}<a href=${`/machines/${replica.id}`} class="text-primary underline"
              >${replica.name || replica.id}</a
            >`,
        )}
      </p>

      <ul class="m-0 pl-5 list-disc text-meta text-muted-foreground">
        <li>Open the build log for this deployment: a build that succeeded can still ship an image that exits at start.</li>
        <li>Open an instance's log and read the first thirty lines. A crash on start is almost always there.</li>
        <li>
          Check the app listens on <code class="font-mono">PORT</code>, which is
          <code class="font-mono">8080</code>. The router dials that port and nothing else.
        </li>
        <li>
          Check the compose file's <code class="font-mono">environment:</code> keys and every
          <code class="font-mono">secret://</code> name. A missing secret fails at start, not at build.
        </li>
      </ul>

      <p class="m-0 text-meta text-muted-foreground">From a terminal:</p>
      <code class="block font-mono text-meta bg-muted rounded-md px-3 py-2"
        >pilot machines logs ${first?.name || first?.id || serviceName}</code
      >
    </div>
  `;
}
