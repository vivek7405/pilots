import { html } from '@webjsdev/core';
import { terminal } from '#site/lib/ui/terminal.ts';
import { section } from '#site/lib/ui/section.ts';
import { PANEL, PROSE, LINK, BTN_PRIMARY, BTN_GHOST, HAIRLINE } from '#site/lib/design/recipes.ts';
import { WORKLOAD_APEX, WEBJS_URL, NEW_TAB } from '#site/lib/links.ts';
import { pageHero } from '#site/lib/ui/page-hero.ts';
import { inlineFact } from '#site/lib/ui/stat.ts';

/**
 * The PaaS face.
 *
 * The reader here already deploys something somewhere and is asking what is
 * different. The answer is not the deploy pipeline, which is table stakes and
 * described plainly below. It is that the thing being deployed is the same
 * primitive as a sandbox, so the two capabilities that normally cost a
 * migration (promote a prototype, preview a pull request) are free.
 */

export const metadata = {
  title: 'Deploy - any Dockerfile to a durable service',
  description:
    'Build any Dockerfile into a microVM, deploy behind a health gate that keeps the old release until the new one answers, and serve it on a custom domain with automatic certificates.',
};

/**
 * Promote, capability by capability.
 *
 * The page said for a long time that promotion changes one number on a row,
 * which is true of the MECHANISM and answers the wrong question: a reader
 * deciding whether to trust this with real traffic wants to know what they
 * get, and the knob panels below show three identical rows out of four. So the
 * table leads and the knobs support it.
 *
 * Promotion is purely ADDITIVE, and that is the fact worth showing plainly:
 * every row that a sandbox has stays exactly as it was, because promote does
 * not rewrite the machine (internal/services/promote.go), and checkpoints,
 * exec, files and volumes are all machine-scoped routes rather than sandbox
 * ones. Nothing here may claim a capability is lost, because none is.
 */
const PROMOTE_ROWS: [string, string, string][] = [
  ['Its URL, its state and its agent token', 'yes', 'unchanged'],
  ['Suspends when idle, wakes on the next request', 'yes', 'unchanged'],
  ['Exec, a terminal, and files in and out', 'yes', 'unchanged'],
  ['Checkpoint, and restore in place', 'yes', 'unchanged'],
  ['A release to deploy, and to roll back to', 'no', 'added'],
  ['Health-gated deploys, the old release serving until the new one answers', 'no', 'added'],
  ['More than one copy, with requests spread across them', 'no', 'added'],
  ['Copies started by load, and stopped when it passes', 'no', 'added'],
  ['A domain of your own, with its certificate', 'no', 'added'],
  ['Its lifecycle managed by', 'the idle monitor', 'the autoscaler'],
];

export default function Deploy() {
  return html`
    ${pageHero({
      heading: 'Ship a Dockerfile, keep the old one running',
      lede: html`Point Pilots at any repository with a Dockerfile and it builds a microVM image, starts it
        behind a health check, and cuts traffic over only once the new release answers. The previous
        release stays alive until then, which is what makes a rollback instant rather than a rebuild.`,
      actions: html`<a class=${BTN_PRIMARY} href="/architecture">How it works underneath</a>`,
    })}

    ${section({
      id: 'build',
      heading: 'Any Dockerfile, and structured logs when it fails',
      lede: html`The build turns a container image into a flat filesystem and stores it as a
        content-addressed template, which is what later lets machines start from it without copying
        it. The interesting part is the log format.`,
      body: html`
        <div class="grid gap-8 wide:grid-cols-[1fr_1fr] wide:items-start">
          <div>
            ${terminal('pilot deploy', [
              { kind: 'cmd', text: 'pilot deploy' },
              { kind: 'out', text: 'uploading context' },
              { kind: 'out', text: 'step 3/7  RUN npm ci' },
              { kind: 'out', text: 'step 7/7  flattening to ext4' },
              { kind: 'out', text: 'health check passed, cutting over' },
              { kind: 'mark', text: `checkout.${WORKLOAD_APEX}` },
            ])}
          </div>
          <div class="flex flex-col gap-5">
            <div>
              <p class="font-semibold m-0 mb-1.5">Logs an agent can act on</p>
              <p class="text-sm text-ink-muted m-0">
                Build output streams as structured records rather than as a wall of text, so the
                thing reading it can tell which step failed and why without scraping. That matters
                because the intended reader is often not a person: an agent that can parse the
                failure can patch the Dockerfile and try again.
              </p>
            </div>
            <div>
              <p class="font-semibold m-0 mb-1.5">No Dockerfile? The agent writes one</p>
              <p class="text-sm text-ink-muted m-0">
                Detection is by lockfile and project layout, and the generated file is a starting
                point the build loop then corrects. Django, Rails, Next.js, and anything else that
                boots from a command are all the same problem here.
              </p>
            </div>
            <div>
              <p class="font-semibold m-0 mb-1.5">Two ways in, one pipeline</p>
              <p class="text-sm text-ink-muted m-0">
                Push a local directory from the command line, or connect a repository once and let
                pushes deploy themselves. The webhook is an ordinary route on every host, so there
                is no build service to be down.
              </p>
            </div>
          </div>
        </div>
      `,
    })}

    ${section({
      id: 'cutover',
      layout: 'split',
      heading: 'Nothing takes traffic until it answers',
      lede: html`Waiting a fixed number of seconds and hoping is the usual way a deploy decides it
        worked. Here the new release has to answer its readiness check before it receives any
        traffic, and the old one keeps serving until it does.`,
      body: html`
        <ol class="m-0 p-0 list-none">
          ${[
            ['Start the new release alongside the old', 'Both exist at once. Nothing has moved yet.'],
            ['Wait for readiness', 'The new machine answers its health endpoint, or it does not and the deploy stops here with the old release untouched.'],
            ['Cut traffic over', 'The router points at the new release. The address does not change, because addresses never change.'],
            ['Keep the old release', 'Retained, so rolling back is pointing the router back rather than building anything.'],
          ].map(
            ([t, b], i) => html`
              <li class="grid grid-cols-[2rem_1fr] gap-4 py-5 ${i > 0 ? 'border-t border-rule' : ''}">
                <span class="font-mono text-sm text-ink-subtle">${i + 1}</span>
                <div>
                  <p class="font-semibold m-0">${t}</p>
                  <p class="text-sm text-ink-muted m-0 mt-1 max-w-[70ch]">${b}</p>
                </div>
              </li>
            `,
          )}
        </ol>

        <hr class="${HAIRLINE} my-10" />

        <div class="grid gap-6 mid:grid-cols-3">
          <div>
            <p class="font-semibold m-0 mb-1.5">Custom domains</p>
            <p class="text-sm text-ink-muted m-0">
              Point a record at the fleet and the certificate is issued on demand. Any host can
              answer the challenge, so issuance is not something one machine owns.
            </p>
          </div>
          <div>
            <p class="font-semibold m-0 mb-1.5">Replicas that follow load</p>
            <p class="text-sm text-ink-muted m-0">
              Traffic spread across healthy replicas. A concurrency ceiling starts the next one, and
              excess capacity stops again, down to a floor you set.
            </p>
          </div>
          <div>
            <p class="font-semibold m-0 mb-1.5">Scale to zero, honestly</p>
            <p class="text-sm text-ink-muted m-0">
              A floor of zero is the default for a real service, not a setting to find. The first
              request afterwards is held while the machine comes back rather than being shown a
              splash page, and a database is kept up for as long as a client holds a connection to
              it.
            </p>
          </div>
        </div>
      `,
    })}

    ${section({
      id: 'promote',
      heading: 'What promotion adds',
      lede: html`A sandbox becomes a production service by gaining a release, a health check and a
        number of copies to run. Everything it already did, it still does, at the same address.
        Nothing is rebuilt and nothing is copied, because there was never a second kind of thing to
        copy it into.`,
      body: html`
        <div class="overflow-x-auto scroll-thin">
          <table class="w-full border-collapse text-sm min-w-[640px]">
            <caption class="sr-only">
              What a machine can do as a sandbox, and what it can do once it is a service
            </caption>
            <thead>
              <tr class="border-b border-rule-strong text-left">
                <th scope="col" class="py-3 pr-6 font-mono text-xs uppercase tracking-[0.14em] text-ink-subtle font-medium">Capability</th>
                <th scope="col" class="py-3 pr-6 font-mono text-xs uppercase tracking-[0.14em] text-ink-subtle font-medium">As a sandbox</th>
                <th scope="col" class="py-3 font-mono text-xs uppercase tracking-[0.14em] text-ink-subtle font-medium">After promote</th>
              </tr>
            </thead>
            <tbody>
              ${PROMOTE_ROWS.map(
                ([capability, before, after]) => html`
                  <tr class="border-b border-rule align-top">
                    <td class="py-4 pr-6">${capability}</td>
                    <td class="py-4 pr-6 text-ink-muted whitespace-nowrap">${before}</td>
                    <td class="py-4 whitespace-nowrap">${after}</td>
                  </tr>
                `,
              )}
            </tbody>
          </table>
        </div>

        <p class="${PROSE} mt-10">
          The last row is the whole lifecycle change. Before, the idle monitor decides when the
          machine sleeps, on a timer and on whether anything is still talking to it. After, the
          autoscaler does, and it will not take the last copy down below the floor you set. The
          knobs themselves do not move, which is why a promoted service still suspends when nobody
          is using it.
        </p>

        <div class="grid gap-6 mid:grid-cols-2 mt-8">
          <div class="${PANEL} p-6">
            <p class="font-semibold m-0 mb-2.5">As a sandbox</p>
            <dl class="m-0 grid grid-cols-[auto_1fr] gap-x-6 gap-y-2 font-mono text-sm">
              <dt class="text-ink-subtle">autoStop</dt><dd class="m-0">suspend</dd>
              <dt class="text-ink-subtle">autoStart</dt><dd class="m-0">true</dd>
              <dt class="text-ink-subtle">minRunning</dt><dd class="m-0">0</dd>
              <dt class="text-ink-subtle">replicas</dt><dd class="m-0">one</dd>
            </dl>
          </div>
          <div class="${PANEL} p-6">
            <p class="font-semibold m-0 mb-2.5">After promote</p>
            <dl class="m-0 grid grid-cols-[auto_1fr] gap-x-6 gap-y-2 font-mono text-sm">
              <dt class="text-ink-subtle">autoStop</dt><dd class="m-0">suspend</dd>
              <dt class="text-ink-subtle">autoStart</dt><dd class="m-0">true</dd>
              <dt class="text-ink-subtle">minRunning</dt><dd class="m-0">0</dd>
              <dt class="text-ink-subtle">replicas</dt><dd class="m-0">one or more</dd>
            </dl>
          </div>
        </div>

        <p class="${PROSE} mt-8">
          The same property makes pull request previews close to free. A pull request opens, its
          build becomes a sandbox at its own address, and it suspends when nobody is looking at it,
          which is most of the time. It is destroyed when the branch merges.
        </p>
      `,
    })}

    ${section({
      id: 'data',
      layout: 'split',
      heading: 'Where application data actually belongs',
      lede: html`Checkpoints capture a machine at a moment, which is the wrong granularity for a
        database. Volumes are the right one: durable per write, and not tied to the host that
        happens to be running the machine.`,
      body: html`
        <p class="${PROSE}">
          A machine with a volume mounted stays on its host while it holds it. When that host dies,
          the volume comes back wherever the machine is rescued, because the underlying storage was
          never local to begin with. The root disk is stored the same way, in the same bucket, with
          the host's NVMe as a cache in front of both. What differs is the promise. A volume write is
          durable when it returns, and a root write is durable at the next flush, at most
          ${inlineFact('rootFlushWindow')} later. That is the difference between a place for a
          database and a place for everything else.
        </p>
        <p class="${PROSE} mt-4">
          The fleet's own test battery asserts this rather than assuming it: volume data survives
          the host dying and the machine being rescued elsewhere.
        </p>
      `,
    })}

    <!-- A sign-off line rather than a panel or a heading. Three pages ending
         on three different shapes is the point (AGENTS.md invariant 3). -->
    <div class="max-w-6xl mx-auto px-6 pb-24">
      <hr class="${HAIRLINE} mb-10" />
      <p class="${PROSE} text-lede max-w-[58ch]">
        WebJs apps have no build step, so deploying one is copying files and starting a process,
        and its readiness endpoint is exactly what the health gate above waits on. Neither product
        requires the other. They are designed by people who know what the other one does.
      </p>
      <div class="flex flex-wrap gap-3 mt-7">
        <a class=${BTN_PRIMARY} href=${WEBJS_URL} target="_blank" rel="noopener">Visit WebJs${NEW_TAB}</a>
      </div>
    </div>
  `;
}
