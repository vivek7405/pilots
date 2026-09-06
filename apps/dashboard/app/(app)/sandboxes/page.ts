/**
 * The sandboxes list.
 *
 * The page reads the rows so the list is complete with JavaScript off, and
 * hands them to `<machine-list>` as its starting state. Only the list ships;
 * the heading and the description stay HTML the browser never pays for.
 *
 * The hosts go with them, and they are not decoration: whether a sleeping
 * sandbox resumes warm or starts fresh depends on which hosts are alive and
 * what CPU vendor they are, and that is the one thing the list could not say
 * before.
 *
 * `sandboxes` on the element narrows the rows to machines that belong to no
 * service. An instance of a service is reached from that service, where its
 * deployment and its siblings are, rather than from a flat list that mixes the
 * two products together.
 */
import { html } from '@webjsdev/core';
import { orUnauthorized, requireOrg } from '#modules/auth/session.server.ts';
import { listServicesWithStatus } from '#modules/services/queries/list-services-with-status.server.ts';
import { lede, pageHeading } from '#lib/utils/ui.ts';
import { buttonClass } from '#components/ui/button.ts';
import { cn } from '#lib/utils/cn.ts';
import { NOUN, SLEEP_SENTENCE } from '#lib/vocabulary.ts';
import '#modules/machines/components/machine-list.ts';

export const metadata = { title: 'Sandboxes' };

export default async function SandboxesPage() {
  const ctx = (await requireOrg())!;
  const { machines, hosts, services } = orUnauthorized(await listServicesWithStatus());

  return html`
    <div class="flex flex-wrap items-center gap-3">
      ${pageHeading(NOUN.Sandboxes)}
      <a href="/sandboxes/playground" class=${cn(buttonClass({ size: 'sm' }), 'ml-auto no-underline')}>Open a sandbox</a>
    </div>
    ${lede(
      html`A sandbox is a machine that belongs to no service: yours to open a terminal on, snapshot and turn into a
        service later. ${SLEEP_SENTENCE} These are the ones in <strong>${ctx.org.slug}</strong>.`,
    )}
    <machine-list
      .initial=${machines}
      .hosts=${hosts}
      .services=${services.map((s) => ({ id: s.id, name: s.name }))}
      sandboxes
    ></machine-list>
  `;
}
