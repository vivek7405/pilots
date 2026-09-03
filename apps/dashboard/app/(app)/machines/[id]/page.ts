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
import '#modules/machines/components/log-pane.ts';
import '#modules/machines/components/exec-console.ts';

export async function generateMetadata({ params }: PageProps) {
  return { title: `Machine ${params.id}` };
}

export default async function MachinePage({ params }: PageProps) {
  const ctx = (await requireOrg())!;
  const found = await getMachine({ orgId: ctx.org.id, id: params.id });
  if (!found) throw notFound();
  const { machine, checkpoints } = found;

  return html`
    <div class="flex flex-wrap items-baseline gap-3">
      <h1 class="text-2xl font-semibold tracking-tight m-0">${machine.name || machine.id}</h1>
      <span class="font-mono text-sm text-muted-foreground">${machine.state}</span>
    </div>

    <dl class="mt-4 grid grid-cols-[max-content_1fr] gap-x-6 gap-y-1 text-sm">
      <dt class="text-muted-foreground">Id</dt>
      <dd class="m-0 font-mono">${machine.id}</dd>
      <dt class="text-muted-foreground">Host</dt>
      <dd class="m-0 font-mono">${machine.host_id ?? '-'}</dd>
      <dt class="text-muted-foreground">URL</dt>
      <dd class="m-0">${machine.url ? html`<a href=${machine.url} rel="noopener">${machine.url}</a>` : '-'}</dd>
    </dl>

    <section class="mt-8">
      <h2 class="text-lg font-medium m-0 mb-2">Console</h2>
      <log-pane machine-id=${machine.id}></log-pane>
    </section>

    <section class="mt-8">
      <h2 class="text-lg font-medium m-0 mb-2">Run a command</h2>
      <exec-console machine-id=${machine.id}></exec-console>
    </section>

    <section class="mt-8">
      <h2 class="text-lg font-medium m-0 mb-2">Checkpoints</h2>
      ${checkpoints.length === 0
        ? html`<p class="text-muted-foreground">
            None yet. A checkpoint is copy-on-write metadata, so taking one costs no data copy.
          </p>`
        : html`
            <ul class="text-sm list-none p-0 m-0">
              ${checkpoints.map(
                (c) => html`
                  <li class="border-b border-border py-2 flex flex-wrap gap-x-4">
                    <span class="font-mono">${c.id}</span>
                    <span class="text-muted-foreground">${c.durable ? 'durable' : 'local only'}</span>
                    ${c.comment ? html`<span>${c.comment}</span>` : ''}
                  </li>
                `,
              )}
            </ul>
          `}
      <p class="text-xs text-muted-foreground mt-2">
        A restore is in place: the machine keeps its id and its URL.
      </p>
    </section>
  `;
}
