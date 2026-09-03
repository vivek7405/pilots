/** Volumes, read-only: they come from a deploy, not from a button here. */
import { html } from '@webjsdev/core';
import { requireOrg } from '#modules/auth/session.server.ts';
import { listVolumes } from '#modules/volumes/queries/list-volumes.server.ts';

export const metadata = { title: 'Volumes' };

export default async function VolumesPage() {
  const ctx = (await requireOrg())!;
  const volumes = await listVolumes(ctx.org.id).catch(() => []);

  return html`
    <h1 class="text-2xl font-semibold tracking-tight m-0">Volumes</h1>
    <p class="text-muted-foreground mt-1 mb-6">
      Created by a deploy from the volumes a compose file names.
    </p>
    ${volumes.length === 0
      ? html`<p class="text-muted-foreground">No volumes.</p>`
      : html`
          <div class="overflow-x-auto">
            <table class="w-full text-sm border-collapse">
              <thead>
                <tr class="text-left text-muted-foreground border-b border-border">
                  <th class="py-2 pr-4 font-medium">Name</th>
                  <th class="py-2 pr-4 font-medium">Size</th>
                  <th class="py-2 pr-4 font-medium">Mount</th>
                  <th class="py-2 pr-4 font-medium">Machine</th>
                  <th class="py-2 font-medium">Host</th>
                </tr>
              </thead>
              <tbody>
                ${volumes.map(
                  (v) => html`
                    <tr class="border-b border-border">
                      <td class="py-2 pr-4">${v.name}</td>
                      <td class="py-2 pr-4">${v.size_gib} GiB</td>
                      <td class="py-2 pr-4 font-mono">${v.mount_path}</td>
                      <td class="py-2 pr-4 font-mono text-muted-foreground">${v.machine_id ?? '-'}</td>
                      <td class="py-2 font-mono text-muted-foreground">${v.host_id ?? '-'}</td>
                    </tr>
                  `,
                )}
              </tbody>
            </table>
          </div>
        `}
  `;
}
