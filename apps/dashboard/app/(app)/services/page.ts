/**
 * The services list.
 *
 * There is no "new service" form. A service is created by `pilot deploy` or by
 * promoting a sandbox, both of which carry a build the dashboard does not have.
 */
import { html } from '@webjsdev/core';
import { requireOrg } from '#modules/auth/session.server.ts';
import { listServices } from '#modules/services/queries/list-services.server.ts';

export const metadata = { title: 'Services' };

export default async function ServicesPage() {
  const ctx = (await requireOrg())!;
  const services = await listServices(ctx.org.id).catch(() => []);

  return html`
    <h1 class="text-2xl font-semibold tracking-tight m-0">Services</h1>
    <p class="text-muted-foreground mt-1 mb-6">
      Created by <code>pilot deploy</code> or by promoting a sandbox. A promote keeps the URL.
    </p>
    ${services.length === 0
      ? html`<p class="text-muted-foreground">No services.</p>`
      : html`
          <div class="overflow-x-auto">
            <table class="w-full text-sm border-collapse">
              <thead>
                <tr class="text-left text-muted-foreground border-b border-border">
                  <th class="py-2 pr-4 font-medium">Name</th>
                  <th class="py-2 pr-4 font-medium">Replicas</th>
                  <th class="py-2 pr-4 font-medium">Repo</th>
                  <th class="py-2 font-medium">URL</th>
                </tr>
              </thead>
              <tbody>
                ${services.map(
                  (s) => html`
                    <tr class="border-b border-border">
                      <td class="py-2 pr-4"><a href=${`/services/${s.id}`} class="text-foreground">${s.name}</a></td>
                      <td class="py-2 pr-4">${s.replicas}</td>
                      <td class="py-2 pr-4 text-muted-foreground">
                        ${s.repo ? `${s.repo}${s.branch ? `#${s.branch}` : ''}` : '-'}
                      </td>
                      <td class="py-2">
                        ${s.custom_domain
                          ? html`<a href=${`https://${s.custom_domain}`} rel="noopener">${s.custom_domain}</a>`
                          : s.url
                            ? html`<a href=${s.url} rel="noopener">${s.url}</a>`
                            : '-'}
                      </td>
                    </tr>
                  `,
                )}
              </tbody>
            </table>
          </div>
        `}
  `;
}
