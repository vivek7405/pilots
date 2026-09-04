/**
 * The machines list.
 *
 * The page reads the rows so the list is complete with JavaScript off, and
 * hands them to `<machine-list>` as its starting state. Only the list ships;
 * the heading and the description stay HTML the browser never pays for.
 */
import { html } from '@webjsdev/core';
import { orUnauthorized, requireOrg } from '#modules/auth/session.server.ts';
import { listMachines } from '#modules/machines/queries/list-machines.server.ts';
import { lede, pageHeading } from '#lib/utils/ui.ts';
import '#modules/machines/components/machine-list.ts';

export const metadata = { title: 'Machines' };

export default async function MachinesPage() {
  const ctx = (await requireOrg())!;
  const machines = orUnauthorized(await listMachines().catch(() => []));

  return html`
    ${pageHeading('Machines')}
    ${lede(html`Sandboxes and service instances in <strong>${ctx.org.slug}</strong>. The list updates as they change.`)}
    <machine-list .initial=${machines}></machine-list>
  `;
}
