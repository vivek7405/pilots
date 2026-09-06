/** Storage, read-only: it comes from a deploy, not from a button here. */
import { html } from '@webjsdev/core';
import type { Volume } from '@pilots/sdk';
import { orUnauthorized, requireOrg } from '#modules/auth/session.server.ts';
import { listVolumes } from '#modules/volumes/queries/list-volumes.server.ts';
import { dataTable, emptyState, lede, pageHeading } from '#lib/utils/ui.ts';
import { NOUN } from '#lib/vocabulary.ts';
import '#components/copy-button.ts';

export const metadata = { title: 'Storage' };

export default async function StoragePage() {
  const ctx = (await requireOrg())!;
  const volumes = orUnauthorized(await listVolumes().catch(() => []));

  return html`
    ${pageHeading(NOUN.Storage)}
    ${lede(
      html`A disk that outlives the service it is attached to, created by a deploy from what a compose file names. These
        are the ones in <strong>${ctx.org.slug}</strong>.`,
    )}
    ${volumes.length === 0
      ? emptyState('No storage yet. It is declared in the compose file and created with the service that mounts it.', {
          command: 'pilot deploy',
        })
      : dataTable<Volume>({
          caption: 'Storage in this team',
          rows: volumes,
          columns: [
            { header: 'Name', cell: (v) => v.name },
            { header: 'Size', align: 'right', cellClass: 'tabular-nums', cell: (v) => `${v.size_gib} GiB` },
            { header: 'Mounted at', cellClass: 'font-mono', cell: (v) => v.mount_path },
            {
              header: 'Attached to',
              cellClass: 'font-mono text-muted-foreground',
              cell: (v) => (v.machine_id ? html`<a href=${`/machines/${v.machine_id}`}>${v.machine_id}</a>` : '-'),
            },
          ],
        })}
  `;
}
