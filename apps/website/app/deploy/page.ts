import { html } from '@webjsdev/core';
import { terminal } from '#lib/ui/terminal.ts';
import { section } from '#lib/ui/section.ts';
import { PANEL, PROSE, LINK, FIELD_LABEL, BTN_PRIMARY, BTN_GHOST, HAIRLINE } from '#lib/design/recipes.ts';
import { WORKLOAD_APEX, WEBJS_URL, GH_URL, NEW_TAB } from '#lib/links.ts';
import { pageHero } from '#lib/ui/page-hero.ts';

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
  title: 'Deploy: any Dockerfile to a durable service',
  description:
    'Build any Dockerfile into a microVM, deploy behind a health gate that keeps the old release until the new one answers, and serve it on a custom domain with automatic certificates.',
};

export default function Deploy() {
  return html`
    ${pageHero({
      eyebrow: 'Services',
      heading: 'Ship a Dockerfile, keep the old one running',
      lede: html`Point pilots at any repository with a Dockerfile and it builds a microVM image, starts it
        behind a health check, and cuts traffic over only once the new release answers. The previous
        release stays alive until then, which is what makes a rollback instant rather than a rebuild.`,
      actions: html`<a class=${BTN_PRIMARY} href="/architecture">How it works underneath</a>`,
    })}

    ${section({
      id: 'build',
      eyebrow: 'Build',
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
              <p class="${FIELD_LABEL} m-0 mb-2">Logs an agent can act on</p>
              <p class="text-sm text-ink-muted m-0">
                Build output streams as structured records rather than as a wall of text, so the
                thing reading it can tell which step failed and why without scraping. That matters
                because the intended reader is often not a person: an agent that can parse the
                failure can patch the Dockerfile and try again.
              </p>
            </div>
            <div>
              <p class="${FIELD_LABEL} m-0 mb-2">No Dockerfile? The agent writes one</p>
              <p class="text-sm text-ink-muted m-0">
                Detection is by lockfile and project layout, and the generated file is a starting
                point the build loop then corrects. Django, Rails, Next.js, and anything else that
                boots from a command are all the same problem here.
              </p>
            </div>
            <div>
              <p class="${FIELD_LABEL} m-0 mb-2">Two ways in, one pipeline</p>
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
      eyebrow: 'Release',
      heading: 'The gate is the readiness endpoint, not a timer',
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
            <p class="${FIELD_LABEL} m-0 mb-2">Custom domains</p>
            <p class="text-sm text-ink-muted m-0">
              Point a record at the fleet and the certificate is issued on demand. Any host can
              answer the challenge, so issuance is not something one machine owns.
            </p>
          </div>
          <div>
            <p class="${FIELD_LABEL} m-0 mb-2">Replicas that follow load</p>
            <p class="text-sm text-ink-muted m-0">
              Traffic spread across healthy replicas; a concurrency ceiling starts the next one, and
              excess capacity stops again, down to a floor you set.
            </p>
          </div>
          <div>
            <p class="${FIELD_LABEL} m-0 mb-2">Scale to zero, honestly</p>
            <p class="text-sm text-ink-muted m-0">
              A floor of zero is allowed for a real service. The first request afterwards is held
              while the machine comes back rather than being shown a splash page.
            </p>
          </div>
        </div>
      `,
    })}

    ${section({
      id: 'promote',
      eyebrow: 'The part nobody else has',
      heading: 'Promotion is a change of intent, not a migration',
      lede: html`A prototype becomes a production service by changing three numbers on its row:
        whether it stops when idle, whether it starts on demand, and how many copies stay running.
        Nothing is rebuilt and nothing is copied, because there was never a second kind of thing to
        copy it into.`,
      body: html`
        <div class="grid gap-6 mid:grid-cols-2">
          <div class="${PANEL} p-6">
            <p class="${FIELD_LABEL} m-0 mb-3">As a sandbox</p>
            <dl class="m-0 grid grid-cols-[auto_1fr] gap-x-6 gap-y-2 font-mono text-sm">
              <dt class="text-ink-subtle">autoStop</dt><dd class="m-0">suspend</dd>
              <dt class="text-ink-subtle">autoStart</dt><dd class="m-0">true</dd>
              <dt class="text-ink-subtle">minRunning</dt><dd class="m-0">0</dd>
            </dl>
          </div>
          <div class="${PANEL} p-6">
            <p class="${FIELD_LABEL} m-0 mb-3">After promote</p>
            <dl class="m-0 grid grid-cols-[auto_1fr] gap-x-6 gap-y-2 font-mono text-sm">
              <dt class="text-ink-subtle">autoStop</dt><dd class="m-0">off</dd>
              <dt class="text-ink-subtle">autoStart</dt><dd class="m-0">true</dd>
              <dt class="text-ink-subtle">minRunning</dt><dd class="m-0">1</dd>
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
      eyebrow: 'State',
      heading: 'Where application data actually belongs',
      lede: html`Checkpoints capture a machine at a moment, which is the wrong granularity for a
        database. Volumes are the right one: durable per write, and not tied to the host that
        happens to be running the machine.`,
      body: html`
        <p class="${PROSE}">
          A machine with a volume mounted stays on its host while it holds it. When that host dies,
          the volume comes back wherever the machine is rescued, because the underlying storage was
          never local to begin with. That is the difference between disk state that survives a host
          failure and disk state that merely survives a restart.
        </p>
        <p class="${PROSE} mt-4">
          Volumes are Phase 5 work and are being built now.
          <a class=${LINK} href="/roadmap">The roadmap</a> tracks it honestly.
        </p>
      `,
    })}

    <div class="max-w-6xl mx-auto px-6 pb-24">
      <div class="rounded border border-rule-strong bg-paper-elev p-8 mid:p-12">
        <h2 class="text-h2 font-bold m-0 max-w-[28ch]">Built by the people who build the framework</h2>
        <p class="${PROSE} mt-4">
          WebJs apps have no build step, so deploying one is copying files and starting a process,
          and its readiness endpoint is exactly what the health gate above waits on. Neither product
          requires the other. They are just designed with knowledge of each other.
        </p>
        <div class="flex flex-wrap gap-3 mt-7">
          <a class=${BTN_PRIMARY} href="/architecture">Read the architecture</a>
          <a class=${BTN_GHOST} href=${WEBJS_URL} target="_blank" rel="noopener">Visit WebJs${NEW_TAB}</a>
          <a class=${BTN_GHOST} href=${GH_URL} target="_blank" rel="noopener">Source${NEW_TAB}</a>
        </div>
      </div>
    </div>
  `;
}
