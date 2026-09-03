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
import { toJson } from '#modules/usage/export.server.ts';
import { resolvePeriod } from '#modules/usage/period.ts';
import { fleet } from '#modules/fleet/client.server.ts';
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

  return html`
    <h1 class="text-2xl font-semibold tracking-tight m-0">Usage</h1>
    <p class="text-muted-foreground mt-1 mb-6">
      Metered on the hosts, aggregated here. The dashboard being down does not stop metering.
    </p>

    <form method="GET" class="flex flex-wrap items-end gap-3 mb-6">
      <label class="text-sm">
        <span class="block text-muted-foreground">From</span>
        <input name="since" type="date" value=${day(since)} class="mt-1 rounded-md border border-border bg-background px-2 py-1">
      </label>
      <label class="text-sm">
        <span class="block text-muted-foreground">To</span>
        <input name="until" type="date" value=${day(until)} class="mt-1 rounded-md border border-border bg-background px-2 py-1">
      </label>
      <button type="submit" class="rounded-md border border-border px-3 py-1.5 text-sm hover:bg-muted">Apply</button>
      <a href=${`/api/usage?${query}&format=csv`} class="text-sm">Download CSV</a>
      <a href=${`/api/usage?${query}`} class="text-sm">JSON</a>
    </form>

    <dl class="grid grid-cols-2 sm:grid-cols-4 gap-4 mb-8">
      ${[
        ['Machine seconds', totals.machine_seconds],
        ['vCPU seconds', totals.vcpu_seconds],
        ['MiB seconds', totals.mib_seconds],
        ['Volume GiB seconds', totals.volume_gib_seconds],
      ].map(
        ([label, value]) => html`
          <div class="rounded-md border border-border p-3">
            <dt class="text-xs text-muted-foreground m-0">${label}</dt>
            <dd class="m-0 text-lg font-medium tabular-nums">${Number(value).toLocaleString('en-US')}</dd>
          </div>
        `,
      )}
    </dl>

    <h2 class="text-lg font-medium m-0 mb-2">Samples</h2>
    ${rows.length === 0
      ? html`<p class="text-muted-foreground">Nothing recorded for this period.</p>`
      : html`
          <div class="overflow-x-auto mb-8">
            <table class="w-full text-sm border-collapse">
              <thead>
                <tr class="text-left text-muted-foreground border-b border-border">
                  <th class="py-2 pr-4 font-medium">Host</th>
                  <th class="py-2 pr-4 font-medium">Window</th>
                  <th class="py-2 pr-4 font-medium text-right">Machine s</th>
                  <th class="py-2 pr-4 font-medium text-right">vCPU s</th>
                  <th class="py-2 pr-4 font-medium text-right">MiB s</th>
                  <th class="py-2 font-medium text-right">Volume GiB s</th>
                </tr>
              </thead>
              <tbody>
                ${rows.map(
                  (r) => html`
                    <tr class="border-b border-border">
                      <td class="py-2 pr-4 font-mono">${r.hostId}</td>
                      <td class="py-2 pr-4 text-muted-foreground">
                        ${r.windowStart.toISOString().slice(0, 16).replace('T', ' ')}
                      </td>
                      <td class="py-2 pr-4 text-right tabular-nums">${r.machineSeconds}</td>
                      <td class="py-2 pr-4 text-right tabular-nums">${r.vcpuSeconds}</td>
                      <td class="py-2 pr-4 text-right tabular-nums">${r.mibSeconds}</td>
                      <td class="py-2 text-right tabular-nums">${r.volumeGibSeconds}</td>
                    </tr>
                  `,
                )}
              </tbody>
            </table>
          </div>
        `}

    <h2 class="text-lg font-medium m-0 mb-2">Fleet</h2>
    <hosts-strip .initial=${hosts}></hosts-strip>
  `;
}
