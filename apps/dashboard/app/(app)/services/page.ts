/**
 * The services list.
 *
 * Name, Replicas, Repo and URL were four facts and none of them was whether
 * the thing is up, which is the question a reader opens this page with. The
 * status column answers it from fields the engine already returns.
 *
 * A service is created by a deploy that carries a build, which this page
 * cannot make; /services/new explains the one command that can.
 */
import { html } from '@webjsdev/core';
import { orUnauthorized, requireOrg } from '#modules/auth/session.server.ts';
import { listServicesWithStatus } from '#modules/services/queries/list-services-with-status.server.ts';
import { serviceStatusLine } from '#modules/services/utils/ui/status-line.ts';
import { buttonClass } from '#components/ui/button.ts';
import { dataTable, emptyState, lede, pageHeading } from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';
import type { Service } from '@pilots/sdk';
import type { Machine as BrowserMachine } from '#modules/machines/types.ts';
// The empty state carries a command with a copy control, so this page ships
// the element that control is: an un-imported custom element never upgrades
// and the button it renders does nothing when clicked.
import '#components/copy-button.ts';
import '#components/link-rows.ts';
import '#components/list-filter.ts';
import '#components/relative-time.ts';

export const metadata = { title: 'Services' };

/**
 * Where a custom domain opens.
 *
 * The scheme is NOT hardcoded to https: a custom domain is served by the same
 * listener the service's own URL names, so on a fleet without TLS -- the local
 * single box, the three-node rig -- it opens over http on the plain listener's
 * port. hostd already decides that once and renders it into `url` (see
 * `internal/api/publicurl.go`), so read the shape off that rather than assume
 * a second one here. https when the service has no URL to read.
 */
function customDomainHref(service: Service): string {
  if (!service.url) return `https://${service.custom_domain}`;
  try {
    const base = new URL(service.url);
    return `${base.protocol}//${service.custom_domain}${base.port ? `:${base.port}` : ''}`;
  } catch {
    return `https://${service.custom_domain}`;
  }
}

export default async function ServicesPage() {
  const ctx = (await requireOrg())!;
  const { services, machines, releases } = orUnauthorized(await listServicesWithStatus());
  const replicasOf = (id: string) => machines.filter((m) => m.service_id === id) as BrowserMachine[];

  return html`
    <div class="flex flex-wrap items-center gap-3">
      ${pageHeading('Services')}
      <a href="/services/new" class=${cn(buttonClass({ size: 'sm' }), 'ml-auto')}>New service</a>
    </div>
    ${lede(html`Created by <code class="font-mono">pilot deploy</code> or by promoting a sandbox. A promote keeps the
    URL.`)}
    ${services.length === 0
      ? emptyState('No services yet. A deploy from a repository with a compose file creates the first one.', {
          command: 'pilot deploy',
          href: '/services/new',
          label: 'How to deploy',
        })
      : html`
          <div class="mb-3 flex justify-end">
            <list-filter for="services-table" placeholder="Filter services"></list-filter>
          </div>
          <link-rows>
            ${dataTable<Service>({
              id: 'services-table',
              caption: 'Services in this organisation',
              rows: services,
              rowHref: (s) => `/services/${s.id}`,
              columns: [
                {
                  header: 'Name',
                  cell: (s) => html`<a href=${`/services/${s.id}`} class="text-foreground">${s.name}</a>`,
                },
                {
                  header: 'Status',
                  cell: (s) => serviceStatusLine(s, replicasOf(s.id), releases[s.id] ?? []),
                },
                { header: 'App', cellClass: 'text-muted-foreground', cell: (s) => s.app ?? '' },
                {
                  header: 'Replicas',
                  align: 'right',
                  cellClass: 'tabular-nums',
                  cell: (s) => s.replicas,
                },
                {
                  header: 'URL',
                  cell: (s) =>
                    s.custom_domain
                      ? html`<a href=${customDomainHref(s)} rel="noopener">${s.custom_domain}</a>`
                      : s.url
                        ? html`<a href=${s.url} rel="noopener">${s.url}</a>`
                        : '',
                },
                {
                  header: 'Last deploy',
                  cell: (s) => {
                    const at = (releases[s.id] ?? []).find((r) => r.id === s.release_id)?.created_at;
                    return at ? html`<relative-time datetime=${String(at)}></relative-time>` : '';
                  },
                },
              ],
            })}
          </link-rows>
        `}
  `;
}
