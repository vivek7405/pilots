/** Custom domains, and the CNAME each one needs. */
import { html } from '@webjsdev/core';
import type { PageProps } from '@webjsdev/core';
import { orUnauthorized, requireOrg } from '#modules/auth/session.server.ts';
import { listDomains } from '#modules/domains/queries/list-domains.server.ts';
import type { DomainRow } from '#modules/domains/queries/list-domains.server.ts';
import { listServices } from '#modules/services/queries/list-services.server.ts';
import { addDomain } from '#modules/domains/actions/add-domain.server.ts';
import { deleteDomain } from '#modules/domains/actions/delete-domain.server.ts';
import { badgeClass } from '#components/ui/badge.ts';
import { buttonClass } from '#components/ui/button.ts';
import { inputClass } from '#components/ui/input.ts';
import { nativeSelectClass, nativeSelectIconClass, nativeSelectWrapperClass } from '#components/ui/native-select.ts';
import {
  dataTable,
  emptyState,
  errorAlert,
  field,
  formRowClass,
  lede,
  pageHeading,
  sectionHeading,
} from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';
import type { Service } from '@pilots/sdk';

export const metadata = { title: 'Domains' };

export default async function DomainsPage({ actionData }: PageProps) {
  const ctx = (await requireOrg())!;
  const [rows, services] = (
    await Promise.all([listDomains().catch(() => []), listServices().catch(() => [])])
  ).map(orUnauthorized) as [DomainRow[], Service[]];
  const errors = (actionData as { fieldErrors?: Record<string, string>; error?: string } | undefined) ?? {};

  return html`
    ${pageHeading('Domains')}
    ${lede('A certificate is issued over HTTP-01, which any host in the fleet can answer.')}
    ${errors.error ? errorAlert(errors.error) : ''}
    <!-- One wrapper around BOTH branches, so the gap before the form below does
         not disappear when the list is empty. -->
    <div class="mb-8">
      ${rows.length === 0
        ? emptyState('No custom domains.')
        : html`
            ${dataTable<DomainRow>({
              caption: 'Custom domains in this organisation',
              rows,
              columns: [
                { header: 'Hostname', cell: ({ domain }) => domain.hostname },
                {
                  header: 'Service',
                  cell: ({ service }) =>
                    service ? html`<a href=${`/services/${service.id}`} class="text-foreground">${service.name}</a>` : '-',
                },
                {
                  header: 'Certificate',
                  cell: ({ domain }) =>
                    html`<span class=${badgeClass({ variant: domain.verified ? 'secondary' : 'outline' })}
                      >${domain.verified ? 'verified' : 'pending'}</span
                    >`,
                },
                {
                  header: 'CNAME target',
                  cellClass: 'font-mono text-muted-foreground',
                  cell: ({ domain }) => domain.cname_target,
                },
                {
                  header: 'Actions',
                  headerHidden: true,
                  align: 'right',
                  cell: ({ domain }) => html`
                    <form action=${deleteDomain}>
                      <input type="hidden" name="hostname" value=${domain.hostname}>
                      <button type="submit" class=${buttonClass({ variant: 'outline', size: 'sm' })}>Remove</button>
                    </form>
                  `,
                },
              ],
            })}
          `}
    </div>

    ${sectionHeading('Add a domain')}
    <form action=${addDomain} class=${formRowClass()}>
      ${field({
        id: 'hostname',
        label: 'Hostname',
        error: errors.fieldErrors?.hostname,
        control: html`<input
          id="hostname"
          name="hostname"
          placeholder="app.example.com"
          required
          aria-invalid=${errors.fieldErrors?.hostname ? 'true' : 'false'}
          class=${inputClass()}
        >`,
      })}
      ${field({
        id: 'domain-service',
        label: 'Service',
        control: html`
          <div class=${cn(nativeSelectWrapperClass(), 'w-full')}>
            <select id="domain-service" name="service" required class=${nativeSelectClass()}>
              ${services.map((s) => html`<option value=${s.id}>${s.name}</option>`)}
            </select>
            <svg class=${nativeSelectIconClass()} viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m6 9 6 6 6-6" /></svg>
          </div>
        `,
      })}
      <button type="submit" class=${buttonClass()}>Add</button>
    </form>
  `;
}
