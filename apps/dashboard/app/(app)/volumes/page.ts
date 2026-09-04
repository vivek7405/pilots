/** Volumes, read-only: they come from a deploy, not from a button here. */
import { html } from '@webjsdev/core';
import type { Volume } from '@pilots/sdk';
import { requireOrg } from '#modules/auth/session.server.ts';
import { listVolumes } from '#modules/volumes/queries/list-volumes.server.ts';
import { dataTable, emptyState, lede, pageHeading } from '#lib/utils/ui.ts';

export const metadata = { title: 'Volumes' };

export default async function VolumesPage() {
  const ctx = (await requireOrg())!;
  const volumes = await listVolumes(ctx.org.id).catch(() => []);

  return html`
    ${pageHeading('Volumes')} ${lede('Created by a deploy from the volumes a compose file names.')}
    ${volumes.length === 0
      ? emptyState('No volumes.')
      : dataTable<Volume>({
          caption: 'Volumes in this organisation',
          rows: volumes,
          columns: [
            { header: 'Name', cell: (v) => v.name },
            { header: 'Size', align: 'right', cellClass: 'tabular-nums', cell: (v) => `${v.size_gib} GiB` },
            { header: 'Mount', cellClass: 'font-mono', cell: (v) => v.mount_path },
            { header: 'Machine', cellClass: 'font-mono text-muted-foreground', cell: (v) => v.machine_id ?? '-' },
            { header: 'Host', cellClass: 'font-mono text-muted-foreground', cell: (v) => v.host_id ?? '-' },
          ],
        })}
  `;
}
