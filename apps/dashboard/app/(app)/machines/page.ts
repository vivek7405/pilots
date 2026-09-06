/**
 * The machines list.
 *
 * The page reads the rows so the list is complete with JavaScript off, and
 * hands them to `<machine-list>` as its starting state. Only the list ships;
 * the heading and the description stay HTML the browser never pays for.
 *
 * The hosts go with them, and they are not decoration: whether a suspended
 * machine resumes warm or has to cold-boot depends on which hosts are alive
 * and what CPU vendor they are, and that is the one thing the list could not
 * say before.
 */
import { html } from '@webjsdev/core';
import { orUnauthorized, requireOrg } from '#modules/auth/session.server.ts';
import { listServicesWithStatus } from '#modules/services/queries/list-services-with-status.server.ts';
import { lede, pageHeading } from '#lib/utils/ui.ts';
import '#modules/machines/components/machine-list.ts';

export const metadata = { title: 'Machines' };

export default async function MachinesPage() {
  const ctx = (await requireOrg())!;
  const { machines, hosts, services } = orUnauthorized(await listServicesWithStatus());

  return html`
    ${pageHeading('Machines')}
    ${lede(html`Sandboxes and service instances in <strong>${ctx.org.slug}</strong>. The list updates as they change.`)}
    <machine-list
      .initial=${machines}
      .hosts=${hosts}
      .services=${services.map((s) => ({ id: s.id, name: s.name }))}
    ></machine-list>
  `;
}
