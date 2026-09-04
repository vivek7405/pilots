/**
 * The services list.
 *
 * There is no "new service" form. A service is created by `pilot deploy` or by
 * promoting a sandbox, both of which carry a build the dashboard does not have.
 */
import { html } from '@webjsdev/core';
import { requireOrg } from '#modules/auth/session.server.ts';
import { listServices } from '#modules/services/queries/list-services.server.ts';
import { dataTable, emptyState, lede, pageHeading } from '#lib/utils/ui.ts';
import type { Service } from '@pilots/sdk';

export const metadata = { title: 'Services' };

export default async function ServicesPage() {
  const ctx = (await requireOrg())!;
  const services = await listServices(ctx.org.id).catch(() => []);

  return html`
    ${pageHeading('Services')}
    ${lede(html`Created by <code class="font-mono">pilot deploy</code> or by promoting a sandbox. A promote keeps the
    URL.`)}
    ${services.length === 0
      ? emptyState('No services.')
      : dataTable<Service>({
          caption: 'Services in this organisation',
          rows: services,
          columns: [
            {
              header: 'Name',
              cell: (s) => html`<a href=${`/services/${s.id}`} class="text-foreground">${s.name}</a>`,
            },
            { header: 'Replicas', align: 'right', cellClass: 'tabular-nums', cell: (s) => s.replicas },
            {
              header: 'Repo',
              cellClass: 'text-muted-foreground',
              cell: (s) => (s.repo ? `${s.repo}${s.branch ? `#${s.branch}` : ''}` : '-'),
            },
            {
              header: 'URL',
              cell: (s) =>
                s.custom_domain
                  ? html`<a href=${`https://${s.custom_domain}`} rel="noopener">${s.custom_domain}</a>`
                  : s.url
                    ? html`<a href=${s.url} rel="noopener">${s.url}</a>`
                    : '-',
            },
          ],
        })}
  `;
}
