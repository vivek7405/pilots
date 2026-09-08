/**
 * Logs: every running instance's console, live, in one place.
 *
 * The sources are the org's running machines, named by the service they serve
 * or "Sandbox" when they serve none. `<log-stream>` opens one follow per source
 * client-side; there is no aggregating endpoint, because nothing aggregates on
 * a host. With scripting off each instance's raw log is a plain link to the
 * `text/plain` route, so the page is never a dead end.
 */
import { html } from '@webjsdev/core';
import { currentUser } from '#modules/auth/queries/current-user.server.ts';
import { listServicesWithStatus } from '#modules/services/queries/list-services-with-status.server.ts';
import { isSignedOut } from '#modules/auth/session.server.ts';
import { signInLink } from '#modules/auth/sign-in-link.ts';
import { lede, pageHeading, sectionEmpty } from '#lib/utils/ui.ts';
import { NOUN } from '#lib/vocabulary.ts';
import type { LogSource } from '#modules/logs/components/log-stream.ts';
import type { Machine, Service } from '@pilots/sdk';
import '#modules/logs/components/log-stream.ts';

export const metadata = { title: 'Logs' };

/** A machine has a guest that can be tailed once it is up, not before. */
const LIVE = new Set(['running', 'starting']);

export default async function LogsPage() {
  const me = await currentUser();
  if (!me) {
    return html`${pageHeading(NOUN.Logs)} ${signInLink()}`;
  }

  const status = await listServicesWithStatus();
  const { services, machines } = isSignedOut(status)
    ? { services: [] as Service[], machines: [] as Machine[] }
    : status;

  const nameOf = new Map(services.map((s) => [s.id, s.name] as const));
  const sources: LogSource[] = machines
    .filter((m) => LIVE.has(m.state))
    .map((m) => ({
      id: m.id,
      service: m.service_id ? (nameOf.get(m.service_id) ?? 'Service') : 'Sandbox',
      name: m.name ?? m.id,
    }));

  return html`
    ${pageHeading(NOUN.Logs)}
    ${lede('Everything your services and sandboxes print, live, in one place.')}
    ${sources.length === 0
      ? sectionEmpty('Nothing is running', {
          text: 'Logs start at an instance’s last boot, so there is nothing to show until something is up.',
          href: '/',
        })
      : html`
          <log-stream .sources=${sources}></log-stream>
          <!-- Scripting off: each instance's raw log is a plain link. -->
          <noscript>
            <ul class="mt-4 list-none p-0 text-body">
              ${sources.map(
                (s) => html`<li class="py-0.5">
                  <a href=${`/api/machines/${s.id}/logs`}>${s.service} · ${s.name}</a>
                </li>`,
              )}
            </ul>
          </noscript>
        `}
  `;
}
