/**
 * Usage for a period, and the two export links.
 *
 * The rows come from `usage_samples`, which the poller fills from every host's
 * own ledger. Reading the table rather than fanning out to the fleet is what
 * makes this page load in one query.
 */
import { html } from '@webjsdev/core';
import type { PageProps } from '@webjsdev/core';
import { requireOrg } from '#modules/auth/session.server.ts';
import { usageForOrg } from '#modules/usage/queries/usage-for-org.server.ts';
import type { UsageSample } from '#db/schema.server.ts';
import { toJson } from '#modules/usage/export.server.ts';
import { resolvePeriod } from '#modules/usage/period.ts';
import { fleet } from '#modules/fleet/client.server.ts';
import { buttonClass } from '#components/ui/button.ts';
import { cardClass, cardContentClass } from '#components/ui/card.ts';
import { inputClass } from '#components/ui/input.ts';
import { dataTable, emptyState, field, formRowClass, lede, pageHeading, sectionHeading } from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';
import '#modules/usage/components/hosts-strip.ts';

export const metadata = { title: 'Usage' };

export default async function UsagePage({ searchParams }: PageProps) {
  const ctx = (await requireOrg())!;
  const { since, until } = resolvePeriod(
    typeof searchParams.since === 'string' ? searchParams.since : null,
    typeof searchParams.until === 'string' ? searchParams.until : null,
  );
  const rows = await usageForOrg({ orgId: ctx.org.id, since, until });
  const { totals } = toJson(rows);
  const hosts = await fleet.hosts.list().catch(() => []);

  const day = (d: Date) => d.toISOString().slice(0, 10);
  const query = `org=${encodeURIComponent(ctx.org.id)}&since=${day(since)}&until=${day(until)}`;
  const number = (value: unknown) => Number(value).toLocaleString('en-US');

  return html`
    ${pageHeading('Usage')}
    ${lede('Metered on the hosts, aggregated here. The dashboard being down does not stop metering.')}

    <form method="GET" class=${cn(formRowClass(), 'mb-6')}>
      ${field({
        id: 'since',
        label: 'From',
        control: html`<input id="since" name="since" type="date" value=${day(since)} class=${inputClass()}>`,
      })}
      ${field({
        id: 'until',
        label: 'To',
        control: html`<input id="until" name="until" type="date" value=${day(until)} class=${inputClass()}>`,
      })}
      <button type="submit" class=${buttonClass()}>Apply</button>
      <a href=${`/api/usage?${query}&format=csv`} class=${cn(buttonClass({ variant: 'outline' }), 'no-underline')}
        >Download CSV</a
      >
      <a href=${`/api/usage?${query}`} class=${cn(buttonClass({ variant: 'ghost' }), 'no-underline')}>JSON</a>
    </form>

    <dl class="grid grid-cols-2 sm:grid-cols-4 gap-4 mb-8 m-0">
      ${[
        ['Machine seconds', totals.machine_seconds],
        ['vCPU seconds', totals.vcpu_seconds],
        ['MiB seconds', totals.mib_seconds],
        ['Volume GiB seconds', totals.volume_gib_seconds],
      ].map(
        ([label, value]) => html`
          <div class=${cardClass({ size: 'sm' })} data-slot="card" data-size="sm">
            <div class=${cardContentClass()}>
              <dt class="text-xs text-muted-foreground m-0">${label}</dt>
              <dd class="m-0 text-lg font-medium tabular-nums">${number(value)}</dd>
            </div>
          </div>
        `,
      )}
    </dl>

    ${sectionHeading('Samples')}
    ${rows.length === 0
      ? emptyState('Nothing recorded for this period.')
      : html`
          <div class="mb-8">
            ${dataTable<UsageSample>({
              caption: 'Usage samples for the selected period',
              rows,
              columns: [
                { header: 'Host', cellClass: 'font-mono', cell: (r) => r.hostId },
                {
                  header: 'Window',
                  cellClass: 'text-muted-foreground tabular-nums',
                  cell: (r) => r.windowStart.toISOString().slice(0, 16).replace('T', ' '),
                },
                { header: 'Machine s', align: 'right', cellClass: 'tabular-nums', cell: (r) => r.machineSeconds },
                { header: 'vCPU s', align: 'right', cellClass: 'tabular-nums', cell: (r) => r.vcpuSeconds },
                { header: 'MiB s', align: 'right', cellClass: 'tabular-nums', cell: (r) => r.mibSeconds },
                { header: 'Volume GiB s', align: 'right', cellClass: 'tabular-nums', cell: (r) => r.volumeGibSeconds },
              ],
            })}
          </div>
        `}

    ${sectionHeading('Fleet')}
    <hosts-strip .initial=${hosts}></hosts-strip>
  `;
}
