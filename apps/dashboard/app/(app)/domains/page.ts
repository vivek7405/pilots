/** Custom domains, and the CNAME each one needs. */
import { html } from '@webjsdev/core';
import type { PageProps } from '@webjsdev/core';
import { requireOrg } from '#modules/auth/session.server.ts';
import { listDomains } from '#modules/domains/queries/list-domains.server.ts';
import { listServices } from '#modules/services/queries/list-services.server.ts';
import { addDomain } from '#modules/domains/actions/add-domain.server.ts';
import { deleteDomain } from '#modules/domains/actions/delete-domain.server.ts';

export const metadata = { title: 'Domains' };

export default async function DomainsPage({ actionData }: PageProps) {
  const ctx = (await requireOrg())!;
  const [rows, services] = await Promise.all([
    listDomains(ctx.org.id).catch(() => []),
    listServices(ctx.org.id).catch(() => []),
  ]);
  const errors = (actionData as { fieldErrors?: Record<string, string>; error?: string } | undefined) ?? {};

  return html`
    <h1 class="text-2xl font-semibold tracking-tight m-0">Domains</h1>
    <p class="text-muted-foreground mt-1 mb-6">
      A certificate is issued over HTTP-01, which any host in the fleet can answer.
    </p>

    ${errors.error ? html`<p role="alert" class="text-destructive">${errors.error}</p>` : ''}

    ${rows.length === 0
      ? html`<p class="text-muted-foreground">No custom domains.</p>`
      : html`
          <div class="overflow-x-auto mb-8">
            <table class="w-full text-sm border-collapse">
              <thead>
                <tr class="text-left text-muted-foreground border-b border-border">
                  <th class="py-2 pr-4 font-medium">Hostname</th>
                  <th class="py-2 pr-4 font-medium">Service</th>
                  <th class="py-2 pr-4 font-medium">Verified</th>
                  <th class="py-2 pr-4 font-medium">CNAME target</th>
                  <th class="py-2 font-medium"><span class="sr-only">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                ${rows.map(
                  ({ domain, service }) => html`
                    <tr class="border-b border-border">
                      <td class="py-2 pr-4">${domain.hostname}</td>
                      <td class="py-2 pr-4">
                        ${service
                          ? html`<a href=${`/services/${service.id}`} class="text-foreground">${service.name}</a>`
                          : '-'}
                      </td>
                      <td class="py-2 pr-4">${domain.verified ? 'yes' : 'pending'}</td>
                      <td class="py-2 pr-4 font-mono text-muted-foreground">${domain.cname_target}</td>
                      <td class="py-2 text-right">
                        <form action=${deleteDomain}>
                          <input type="hidden" name="hostname" value=${domain.hostname}>
                          <button type="submit" class="rounded-md border border-border px-2 py-1 hover:bg-muted">
                            Remove
                          </button>
                        </form>
                      </td>
                    </tr>
                  `,
                )}
              </tbody>
            </table>
          </div>
        `}

    <h2 class="text-lg font-medium m-0 mb-2">Add a domain</h2>
    <form action=${addDomain} class="flex flex-wrap items-end gap-3">
      <label class="text-sm">
        <span class="block text-muted-foreground">Hostname</span>
        <input name="hostname" placeholder="app.example.com" required class="mt-1 rounded-md border border-border bg-background px-2 py-1">
      </label>
      <label class="text-sm">
        <span class="block text-muted-foreground">Service</span>
        <select name="service" required class="mt-1 rounded-md border border-border bg-background px-2 py-1">
          ${services.map((s) => html`<option value=${s.id}>${s.name}</option>`)}
        </select>
      </label>
      <button type="submit" class="rounded-md border border-border px-3 py-1.5 text-sm hover:bg-muted">Add</button>
    </form>
    ${errors.fieldErrors?.hostname ? html`<p class="text-sm text-destructive">${errors.fieldErrors.hostname}</p>` : ''}
  `;
}
