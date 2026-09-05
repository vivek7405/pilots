/**
 * The services list.
 *
 * There is no "new service" form. A service is created by `pilot deploy` or by
 * promoting a sandbox, both of which carry a build the dashboard does not have.
 */
import { html } from '@webjsdev/core';
import { orUnauthorized, requireOrg } from '#modules/auth/session.server.ts';
import { listServices } from '#modules/services/queries/list-services.server.ts';
import { dataTable, emptyState, lede, pageHeading } from '#lib/utils/ui.ts';
import type { Service } from '@pilots/sdk';

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
  const services = orUnauthorized(await listServices().catch(() => []));

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
                  ? html`<a href=${customDomainHref(s)} rel="noopener">${s.custom_domain}</a>`
                  : s.url
                    ? html`<a href=${s.url} rel="noopener">${s.url}</a>`
                    : '-',
            },
          ],
        })}
  `;
}
