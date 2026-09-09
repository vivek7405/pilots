/**
 * One machine: its facts, its console, its checkpoints.
 *
 * A machine belonging to another org is a 404 rather than a 403. `notFound()`
 * is legal here because this is a page render; the equivalent route handler
 * returns a Response instead.
 */
import { html, notFound } from '@webjsdev/core';
import type { PageProps } from '@webjsdev/core';
import { orUnauthorized, requireOrg } from '#modules/auth/session.server.ts';
import { getMachine } from '#modules/machines/queries/get-machine.server.ts';
import { statusDot } from '#modules/machines/utils/ui/state.ts';
import { badgeClass } from '#components/ui/badge.ts';
import { buttonClass } from '#components/ui/button.ts';
import { cardClass, cardContentClass } from '#components/ui/card.ts';
import { dataTable, emptyState, footnote, pageHeading, sectionHeading } from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';
import { NOUN } from '#lib/vocabulary.ts';
import '#modules/logs/components/log-stream.ts';
import '#components/terminal/machine-terminal.ts';
import '#components/copy-button.ts';

interface Checkpoint {
  id: string;
  durable: boolean;
  comment?: string | null;
}

export async function generateMetadata({ params }: PageProps) {
  return { title: `Machine ${params.id}` };
}

export default async function MachinePage({ params }: PageProps) {
  const ctx = (await requireOrg())!;
  const found = orUnauthorized(await getMachine({ id: params.id }));
  if (!found) throw notFound();
  const { machine, checkpoints } = found;

  return html`
    <div class="flex flex-wrap items-center gap-3">
      ${pageHeading(machine.name || machine.id)} ${statusDot(machine.state)}
      <a href=${`/machines/${machine.id}/terminal`} class=${cn(buttonClass({ size: 'sm' }), 'ml-auto')}
        >Open a terminal</a
      >
    </div>

    <div class=${cn(cardClass({ size: 'sm' }), 'mt-4')} data-slot="card" data-size="sm">
      <div class=${cardContentClass()}>
        <dl class="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-1 text-meta m-0">
          <dt class="text-muted-foreground">Id</dt>
          <dd class="m-0 font-mono">${machine.id}</dd>
          <dt class="text-muted-foreground">URL</dt>
          <dd class="m-0">${machine.url ? html`<a href=${machine.url} rel="noopener">${machine.url}</a>` : '-'}</dd>
          <dt class="text-muted-foreground">Labels</dt>
          <dd class="m-0 font-mono">${
            Object.keys(machine.labels ?? {}).length
              ? Object.entries(machine.labels ?? {})
                  .sort(([a], [b]) => a.localeCompare(b))
                  .map(([k, v]) => `${k}=${v}`)
                  .join(' ')
              : '-'
          }</dd>
        </dl>
      </div>
    </div>

    <section id="console" class="mt-8">
      ${sectionHeading(NOUN.Logs, 'Everything this instance has printed since it last started. That is all pilots keeps.')}
      <log-stream .sources=${[{ id: machine.id, service: machine.service_id ? 'Instance' : 'Sandbox', name: machine.name ?? machine.id }]}></log-stream>
    </section>

    <section class="mt-8">
      <div class="flex flex-wrap items-center justify-between gap-3">
        ${sectionHeading(NOUN.Terminal, 'A shell inside this instance, as if you had opened one on the box it runs on.')}
        <a href=${`/machines/${machine.id}/terminal`} class="text-meta">Full screen</a>
      </div>
      <div class="h-[24rem] overflow-hidden rounded-md border border-border">
        <machine-terminal machine-id=${machine.id} class="flex h-full min-h-0 flex-col"></machine-terminal>
      </div>
    </section>

    <section class="mt-8">
      ${sectionHeading(NOUN.Snapshots, 'A point you can return to. Restoring one keeps the URL.')}
      ${checkpoints.length === 0
        ? emptyState('None yet. A snapshot records where the disk was, so taking one copies no data and costs almost nothing.', {
            command: `pilot machines checkpoint ${machine.name || machine.id}`,
          })
        : dataTable<Checkpoint>({
            caption: 'Checkpoints of this machine',
            rows: checkpoints,
            columns: [
              { header: 'Checkpoint', cellClass: 'font-mono', cell: (c) => c.id },
              {
                header: 'Storage',
                cell: (c) =>
                  html`<span class=${badgeClass({ variant: c.durable ? 'secondary' : 'outline' })}
                    >${c.durable ? 'durable' : 'local only'}</span
                  >`,
              },
              { header: 'Comment', cellClass: 'text-muted-foreground', cell: (c) => c.comment ?? '' },
            ],
          })}
      ${footnote('Returning to a snapshot happens in place: the URL and the id are kept.')}
    </section>
  `;
}
