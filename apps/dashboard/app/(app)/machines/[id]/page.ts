/**
 * One machine: its facts, its console, its checkpoints.
 *
 * A machine belonging to another org is a 404 rather than a 403. `notFound()`
 * is legal here because this is a page render; the equivalent route handler
 * returns a Response instead.
 */
import { html, notFound } from '@webjsdev/core';
import type { PageProps } from '@webjsdev/core';
import { requireOrg } from '#modules/auth/session.server.ts';
import { getMachine } from '#modules/machines/queries/get-machine.server.ts';
import { stateBadge } from '#modules/machines/utils/ui/state.ts';
import { badgeClass } from '#components/ui/badge.ts';
import { cardClass, cardContentClass } from '#components/ui/card.ts';
import { dataTable, emptyState, footnote, pageHeading, sectionHeading } from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';
import '#modules/machines/components/log-pane.ts';
import '#modules/machines/components/exec-console.ts';

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
  const found = await getMachine({ orgId: ctx.org.id, id: params.id });
  if (!found) throw notFound();
  const { machine, checkpoints } = found;

  return html`
    <div class="flex flex-wrap items-center gap-3">
      ${pageHeading(machine.name || machine.id)} ${stateBadge(machine.state)}
    </div>

    <div class=${cn(cardClass({ size: 'sm' }), 'mt-4')} data-slot="card" data-size="sm">
      <div class=${cardContentClass()}>
        <dl class="grid grid-cols-[max-content_1fr] gap-x-6 gap-y-1 text-sm m-0">
          <dt class="text-muted-foreground">Id</dt>
          <dd class="m-0 font-mono">${machine.id}</dd>
          <dt class="text-muted-foreground">Host</dt>
          <dd class="m-0 font-mono">${machine.host_id ?? '-'}</dd>
          <dt class="text-muted-foreground">URL</dt>
          <dd class="m-0">${machine.url ? html`<a href=${machine.url} rel="noopener">${machine.url}</a>` : '-'}</dd>
        </dl>
      </div>
    </div>

    <section class="mt-8">
      ${sectionHeading('Console')}
      <log-pane machine-id=${machine.id}></log-pane>
    </section>

    <section class="mt-8">
      ${sectionHeading('Run a command')}
      <exec-console machine-id=${machine.id}></exec-console>
    </section>

    <section class="mt-8">
      ${sectionHeading('Checkpoints')}
      ${checkpoints.length === 0
        ? emptyState('None yet. A checkpoint is copy-on-write metadata, so taking one costs no data copy.')
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
      ${footnote('A restore is in place: the machine keeps its id and its URL.')}
    </section>
  `;
}
