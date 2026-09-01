import { html } from '@webjsdev/core';
import { section } from '#lib/ui/section.ts';
import { PROSE, LINK, FIELD_LABEL, BTN_GHOST } from '#lib/design/recipes.ts';
import { inlineFact } from '#lib/ui/stat.ts';
import { GH_URL, GH_BOARD_URL, NEW_TAB } from '#lib/links.ts';
import { pageHero } from '#lib/ui/page-hero.ts';

/**
 * The roadmap.
 *
 * A pre-launch product with no signup has one thing to offer a skeptical
 * reader, which is an accurate account of where it actually is. AGENTS.md
 * invariant 6 says to state the limits; this page is that invariant given a
 * whole route.
 *
 * The honesty has a specific shape: a phase is DONE when its gate passed, not
 * when its code was written. That distinction is the repo's own rule, and
 * reproducing it here is what stops this page from becoming the usual roadmap
 * where everything is perpetually "in progress".
 *
 * The status values are maintained by hand against the issue tracker rather
 * than fetched. A marketing page that queries a project board at request time
 * couples the site's availability to an API that has nothing to do with it,
 * and this page changes roughly once a month.
 */

export const metadata = {
  title: 'Roadmap: what is finished and what is not',
  description:
    'The six phases of building pilots, which gate each one had to pass to close, and exactly which are done. Phases 1 to 4 are closed; Phase 5 is in progress.',
};

type Status = 'done' | 'active' | 'next';

type Phase = {
  n: string;
  title: string;
  status: Status;
  issue: number;
  summary: string;
  /**
   * Markup rather than a string, so a gate quoting a threshold renders it
   * through the sourced-facts registry. Phase 3's gate is three numbers, and
   * the no-slop gate correctly refused them as bare text: a measurement in a
   * data array reaches the reader exactly like one in a template.
   */
  gate: unknown;
};

const PHASES: Phase[] = [
  {
    n: '1',
    title: 'Scaffold and contracts',
    status: 'done',
    issue: 2,
    summary:
      'The repository, the state schema, the HTTP API shape, the guest agent protocol, and the storage layout, all frozen before any parallel work began.',
    gate: 'The contracts land as a written architecture document, reviewed, with the Go types and SQL to match.',
  },
  {
    n: '2',
    title: 'Engine core',
    status: 'done',
    issue: 3,
    summary:
      'One box, end to end: boot, exec in both buffered and streaming form, pause and resume, snapshot and restore, the router with wake-on-request, the idle monitor, named checkpoints, and the isolation layer.',
    gate: 'Create a machine, serve its URL, exec against it, checkpoint, mutate, restore and see the mutation gone with the URL and token unchanged, suspend, wake, destroy, and leave no orphaned processes, namespaces, or ports behind.',
  },
  {
    n: '3',
    title: 'The instant engine',
    status: 'done',
    issue: 4,
    summary:
      'The lazy, content-addressed engine underneath everything Phase 2 built: chunked storage, lazy memory paging, lazy disk, fault-order replay, and checkpoints that resume before they finish uploading.',
    gate: html`Create in ${inlineFact('create')}, wake in ${inlineFact('wake')} against a warm
      cache, hold a checkpoint resume gap under ${inlineFact('checkpoint')}, and keep the entire
      Phase 2 suite green.`,
  },
  {
    n: '4',
    title: 'Cross-host and resilience',
    status: 'done',
    issue: 5,
    summary:
      'A single box becomes a fleet: gossiped CRDT state replacing the local database, an encrypted mesh, any host serving any machine, restore on a host that has never seen the machine, and the self-heal loop.',
    gate: 'Hard-kill a host and its machines return on the survivors with the same URLs and no human involved. Kill the host a client is mid-request against and the next request works. Bootstrap a new host with one command and watch it take traffic.',
  },
  {
    n: '5',
    title: 'Volumes and the PaaS face',
    status: 'active',
    issue: 15,
    summary:
      'Durable volumes, the build path from Dockerfile to microVM image, services with health-gated deploys and rollback, custom domains with automatic certificates, promotion, and replicas that follow load. Split into three tracks being built in parallel.',
    gate: 'A real application deploys to a custom domain with valid TLS, its readiness endpoint gates the cutover, killing it restarts it, rollback works, it scales from zero and back, and volume data survives a host dying and being rescheduled.',
  },
  {
    n: '6',
    title: 'Product surface and sign-off',
    status: 'next',
    issue: 7,
    summary:
      'The dashboard, accounts and API keys, the command line tool, typed SDKs, the agent-facing tool server, metering, quotas, and a hostility suite that replays every known incident class as a test.',
    gate: 'A real product builds, iterates, checkpoints, and promotes an application end to end; the dashboard runs as a service on the platform it describes; and the entire battery is green on the production fleet.',
  },
];

const STATUS_LABEL: Record<Status, string> = {
  done: 'closed',
  active: 'in progress',
  next: 'not started',
};

export default function Roadmap() {
  return html`
    ${pageHero({
      eyebrow: 'Roadmap',
      heading: 'Four phases closed, one being built',
      lede: html`A phase closes when its gate passes, not when its code is written. That is the rule the
        repository holds itself to, so it is the rule this page reports against. The gates are below
        in full, including the ones that have not been met.`,
      actions: html`<a class=${BTN_GHOST} href=${GH_BOARD_URL} target="_blank" rel="noopener">The project board${NEW_TAB}</a>
        <a class=${BTN_GHOST} href=${GH_URL} target="_blank" rel="noopener">The source${NEW_TAB}</a>`,
    })}

    ${section({
      id: 'phases',
      eyebrow: 'Six phases',
      heading: 'Everything ships, nothing waits for a later version',
      lede: html`The order is a build order rather than a release plan. There is no reduced first
        version to be upgraded from later: each phase makes the whole system more capable and none
        of them retire what an earlier phase proved.`,
      body: html`
        <ol class="m-0 p-0 list-none flex flex-col">
          ${PHASES.map(
            (p, i) => html`
              <li class="py-8 ${i > 0 ? 'border-t border-rule' : ''}">
                <div class="grid gap-5 wide:grid-cols-[4rem_1fr_auto] wide:gap-8">
                  <span class="font-mono text-readout leading-none text-ink-subtle">${p.n}</span>

                  <div>
                    <h3 class="text-h3 font-bold m-0">${p.title}</h3>
                    <p class="${PROSE} m-0 mt-2">${p.summary}</p>
                    <div class="mt-4 border-l-2 ${p.status === 'done' ? 'border-signal' : 'border-rule-strong'} pl-4">
                      <p class="${FIELD_LABEL} m-0 mb-1">Gate</p>
                      <p class="text-sm text-ink-muted m-0 max-w-[70ch]">${p.gate}</p>
                    </div>
                  </div>

                  <div class="flex wide:flex-col items-start gap-2 wide:text-right">
                    <span
                      class="font-mono text-[10px] uppercase tracking-[0.14em] px-2 py-1 rounded-[2px] whitespace-nowrap
                             ${p.status === 'done'
                               ? 'bg-signal text-signal-ink'
                               : p.status === 'active'
                                 ? 'border border-rule-strong text-ink'
                                 : 'border border-rule text-ink-subtle'}"
                      >${STATUS_LABEL[p.status]}</span
                    >
                    <a
                      class="font-mono text-xs text-ink-subtle hover:text-ink no-underline whitespace-nowrap"
                      href="${GH_URL}/issues/${p.issue}"
                      target="_blank"
                      rel="noopener"
                      >issue #${p.issue}${NEW_TAB}</a
                    >
                  </div>
                </div>
              </li>
            `,
          )}
        </ol>
      `,
    })}

    ${section({
      id: 'honest',
      eyebrow: 'Where it actually is',
      heading: 'What you should not do with this yet',
      lede: html`The engine works and the fleet heals itself. That is genuinely most of the hard
        part, and it is not the same as being ready for your traffic.`,
      body: html`
        <div class="grid gap-px bg-rule border border-rule rounded overflow-hidden mid:grid-cols-2">
          ${[
            ['Do not put production traffic on it', 'Phase 6 exists precisely to earn that, and it has not started. There is no accounts system, no metering, and no quota enforcement yet.'],
            ['There is nothing to sign up for', 'Accounts and API keys are Phase 6 work. No waiting list is collecting addresses in the meantime.'],
            ['The timings are laptop timings', 'The Phase 3 gate was measured on a development machine, not on the hardware this eventually runs on. Real fleet numbers replace them when there is a fleet.'],
            ['One region, one CPU vendor', 'Both are structural rather than temporary. Snapshots cannot cross a CPU vendor boundary, so the fleet commits to one.'],
          ].map(
            ([t, b]) => html`
              <div class="bg-paper-elev p-5">
                <p class="font-semibold m-0 mb-1.5">${t}</p>
                <p class="text-sm text-ink-muted m-0">${b}</p>
              </div>
            `,
          )}
        </div>

        <p class="${PROSE} mt-10">
          What you can do is read it. The design is written down in full, the code implementing it
          is next to the design, and every phase issue carries the gate it had to pass.
          <a class=${LINK} href="/architecture">Start with the architecture.</a>
        </p>
      `,
    })}
  `;
}
