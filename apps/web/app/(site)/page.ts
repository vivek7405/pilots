import { html } from '@webjsdev/core';
import '#site/components/lifecycle-demo.ts';
import '#site/components/fleet-demo.ts';
import { terminal } from '#site/lib/ui/terminal.ts';
import { section } from '#site/lib/ui/section.ts';
import { readout, inlineFact } from '#site/lib/ui/stat.ts';
import { BTN_PRIMARY, BTN_GHOST, PANEL, PROSE, LINK, FIELD_LABEL } from '#site/lib/design/recipes.ts';
import { WEBJS_URL, WORKLOAD_APEX, NEW_TAB, DASHBOARD_HREF } from '#site/lib/links.ts';

/**
 * The home page.
 *
 * It is written as ONE argument, not a list of features, and the section ledes
 * carry it:
 *
 *   hero            from sandbox to production on the same URL, and nothing is
 *                   rebuilt to get there. The mechanism (one machine, two sets
 *                   of lifecycle knobs) is the NEXT two sections' job: said in
 *                   the hero it reads as "your production service is a sandbox",
 *                   which is the doubt it was meant to settle.
 *   one URL         and that identity survives every lifecycle event
 *   two faces       which is what lets one primitive serve both audiences
 *   instant         the reason a sandbox is usable at all: restore, not boot
 *   no control      the reason a service is trustworthy: nothing central to lose
 *   plane
 *   webjs           the sibling product, for readers who arrived from it
 *
 * There was a "what it cannot do" section here, and it is now docs/honesty.md.
 * This is a page someone reads while deciding whether to try the product, and
 * the CPU-vendor rule and the depth of a diff chain are not decisions they
 * make. The operational limits that DO change someone's mind still have a
 * page, and the pages that sell a capability still say where it stops.
 *
 * EVERY SECTION MUST STAND ALONE. Readers arrive mid-page from a search result
 * or a shared link, so a heading plus its first sentence has to resolve with
 * nothing above it. Test it by covering everything above and reading those two.
 * That rule outranks the hand-off between sections whenever they conflict.
 */

function heroTranscript() {
  return terminal('one machine, from scratch pad to production', [
  { kind: 'cmd', text: 'pilot create --template node' },
  { kind: 'mark', text: `bold-otter.${WORKLOAD_APEX}` },
  { kind: 'cmd', text: 'pilot exec bold-otter -- npm test' },
  { kind: 'out', text: 'PASS  test/checkout.test.js' },
  { kind: 'cmd', text: 'pilot checkpoint bold-otter --name green' },
  { kind: 'out', text: 'checkpoint green (uploading in background)' },
  { kind: 'cmd', text: 'pilot promote bold-otter' },
  { kind: 'mark', text: `bold-otter.${WORKLOAD_APEX}` },
    { kind: 'note', text: 'same address, now a production service' },
  ]);
}

export default function Home() {
  return html`
    <!-- HERO. Left-aligned, with the artifact taking the right half and the
         blueprint grid fading out behind both. Not centred: a centred hero is
         the single most template-shaped layout there is, and this page's
         subject is a machine, so it is drawn like a plan of one. -->
    <div class="relative overflow-hidden border-b border-rule">
      <div class="blueprint absolute inset-0 pointer-events-none" aria-hidden="true"></div>

      <div class="relative max-w-6xl mx-auto px-6 pt-16 pb-20 mid:pt-24 mid:pb-28">
        <div class="grid gap-12 wide:grid-cols-[1.1fr_1fr] wide:gap-14 wide:items-center">
          <div>
            <h1 class="text-display font-bold leading-[0.98] m-0">
              From sandbox to production, on the same URL.
            </h1>

            <p class="${PROSE} text-lede mt-6">
              Run your app in an instant sandbox and it has a live URL. Promote it when it turns
              out to matter. It keeps its URL, its state, and its identity, and nothing is rebuilt
              or redeployed to get there.
            </p>

            <div class="flex flex-wrap gap-3 mt-8">
              <a class=${BTN_PRIMARY} href=${DASHBOARD_HREF} data-no-router>Start a sandbox</a>
              <a class=${BTN_GHOST} href="/architecture">How it works</a>
            </div>
          </div>

          <div class="wide:pl-4">${heroTranscript()}</div>
        </div>
      </div>
    </div>

    ${section({
      id: 'url',
      heading: 'A URL that outlives everything that happens to it',
      lede: html`Most platforms mean "stable until you redeploy". On Pilots the address is part of
        the machine's identity, so suspend, wake, checkpoint, restore, promote, and a host dying
        underneath it all leave the address alone. Drive one yourself and watch the counter.`,
      body: html`<lifecycle-demo></lifecycle-demo>`,
    })}

    <!-- TWO FACES. An asymmetric split rather than a symmetric pair of cards:
         the sandbox face is the one a reader is more likely to have arrived
         for, so it gets the wider column and the transcript.

         The line under them is the glimpse of promote, and it exists because
         the site said "same machine" in five places and never once said what
         the second face GAINS. A reader deciding where to run real traffic
         reads "your production service is a sandbox" out of that silence. One
         sentence here, the table on /deploy. -->
    ${section({
      id: 'faces',
      layout: 'split',
      heading: 'Two things to want, one thing to run',
      lede: html`A sandbox and a production service differ by a release, a health check and how many
        copies to run. Both suspend when idle and wake on the next request. They are not two
        products, and Pilots does not build them as two.`,
      body: html`
        <div class="grid gap-6 wide:grid-cols-[1.25fr_1fr]">
          <div class="${PANEL} p-6 flex flex-col gap-4">
            <div class="flex items-baseline gap-3">
              <h3 class="text-h3 font-bold m-0">Sandboxes</h3>
              <span class="${FIELD_LABEL}">for agents</span>
            </div>
            <p class="${PROSE} m-0">
              An agent needs somewhere to run code it just wrote, and it needs that place to exist
              before the thought finishes. Restore from a template instead of booting, keep a
              checkpoint before every risky step, and roll back when the step goes wrong. The
              machine suspends itself when the agent stops typing and wakes on the next request,
              as a held connection rather than a loading page.
            </p>
            <a class="${LINK} text-sm w-fit" href="/sandboxes">What agents get &rarr;</a>
          </div>

          <div class="${PANEL} p-6 flex flex-col gap-4">
            <div class="flex items-baseline gap-3">
              <h3 class="text-h3 font-bold m-0">Services</h3>
              <span class="${FIELD_LABEL}">for production</span>
            </div>
            <p class="${PROSE} m-0">
              Point it at any Dockerfile and get a running service on a custom domain with a real
              certificate. Deploys are health-gated and keep the old release until the new one
              answers. Replicas start under load and stop when it passes.
            </p>
            <a class="${LINK} text-sm w-fit" href="/deploy">What deploying looks like &rarr;</a>
          </div>
        </div>

        <p class="${PROSE} mt-8">
          Going from one to the other only ever adds. The machine keeps its address, its state and
          its checkpoints, and gains a release to roll back to, a health gate on every deploy, more
          copies under load, and a domain of its own.
          <a class=${LINK} href="/deploy#promote">What promotion adds, line by line &rarr;</a>
        </p>
      `,
    })}

    ${section({
      id: 'instant',
      heading: 'Nothing boots, so nothing waits',
      lede: html`A machine that boots takes as long as its operating system does, which is why
        sandbox products either keep you waiting or keep idle VMs burning money. Pilots restores a
        memory snapshot instead, and pages it in lazily as the guest touches it, so a machine is
        answering before most of its memory has been read.`,
      body: html`
        <div class="grid gap-8 mid:grid-cols-3 mid:gap-6">
          ${readout('createMeasured')} ${readout('wakeMeasured')} ${readout('resumeGapMeasured')}
        </div>

        <p class="${PROSE} mt-10">
          Those are medians from the production fleet: bare-metal Hetzner hosts with the bucket a
          network away, timed by the platform's own end-to-end battery. Hover any number to see
          where it came from, or read
          <a class=${LINK} href="/architecture/internals#numbers">how each one is measured</a>.
        </p>

        <div class="grid gap-6 mid:grid-cols-2 mt-10">
          <div class="${PANEL} p-5">
            <p class="font-semibold m-0 mb-1.5">Lazy memory</p>
            <p class="text-sm text-ink-muted m-0 leading-relaxed">
              A userfaultfd handler serves the guest's page faults straight out of a content-addressed
              blob, and replays the fault order recorded on the previous restore so the next one
              front-runs the guest. The alternative is one object-storage round trip per
              ${inlineFact('block')} page, which is not a slower design so much as a different
              product.
            </p>
          </div>
          <div class="${PANEL} p-5">
            <p class="font-semibold m-0 mb-1.5">Lazy disk</p>
            <p class="text-sm text-ink-muted m-0 leading-relaxed">
              The root disk is a read-through overlay: a template shared by every machine on the
              host underneath, the machine's own dirty blocks on top, and the bucket behind both. No
              machine holds a copy of its disk, and one that changed nothing stores nothing.
            </p>
          </div>
        </div>
      `,
    })}

    ${section({
      id: 'fleet',
      layout: 'split',
      heading: 'There is no control plane to lose',
      lede: html`Every host runs the same three processes and serves the entire API, so no request
        needs a particular machine to be alive. State is a gossiped CRDT that every host holds a
        local replica of, which means a lookup is a read from local disk rather than a call to
        something that might be down. Kill a host below and watch where its machines go.`,
      body: html`
        <fleet-demo></fleet-demo>

        <div class="grid gap-6 mid:grid-cols-3 mt-10">
          <div>
            <p class="font-semibold m-0 mb-1.5">Single writer</p>
            <p class="text-sm text-ink-muted m-0">
              A host writes only rows about its own machines. Last-write-wins merges make a
              violation silent rather than loud, so this one is enforced in review.
            </p>
          </div>
          <div>
            <p class="font-semibold m-0 mb-1.5">Object storage is the truth</p>
            <p class="text-sm text-ink-muted m-0">
              Local NVMe is a cache, for the root disk and the volume alike. The design test is
              that you can wipe any host's disk and lose nothing.
            </p>
          </div>
          <div>
            <p class="font-semibold m-0 mb-1.5">Adding a host</p>
            <p class="text-sm text-ink-muted m-0">
              One script and an IP. It joins the gossip mesh and starts taking traffic. There is no
              scheduler to register with.
            </p>
          </div>
        </div>
      `,
    })}

    ${section({
      id: 'webjs',
      layout: 'split',
      heading: 'WebJs is the framework, Pilots is the platform',
      lede: html`They are built by the same people, the way Next.js and Vercel are. You do not need
        either one to use the other: Pilots runs any Dockerfile, and WebJs deploys anywhere a Node
        process runs. They are just designed by people who know what the other one does.`,
      body: html`
        <div class="${PANEL} p-6 flex flex-col mid:flex-row mid:items-center gap-6 justify-between">
          <p class="${PROSE} m-0">
            A WebJs app has no build step, so deploying one is copying files and starting a process.
            Its readiness endpoint is what Pilots gates a health-checked cutover on.
          </p>
          <a class="${BTN_GHOST} shrink-0" href=${WEBJS_URL} target="_blank" rel="noopener"
            >Visit WebJs${NEW_TAB}</a
          >
        </div>
      `,
    })}

    <!-- CLOSING CTA. The dashboard is the way in; the design is the reason to. -->
    <div class="max-w-6xl mx-auto px-6 pb-24">
      <div class="rounded border border-rule-strong bg-paper-elev p-8 mid:p-12">
        <h2 class="text-h2 font-bold m-0 max-w-[24ch]">Start with a sandbox</h2>
        <p class="${PROSE} mt-4">
          Create one, run your code in it, and keep it if it turns out to matter. Nothing has to be
          decided up front, and nothing is rebuilt when you promote it.
        </p>
        <div class="flex flex-wrap gap-3 mt-7">
          <a class=${BTN_PRIMARY} href=${DASHBOARD_HREF} data-no-router>Open the dashboard</a>
        </div>
      </div>
    </div>
  `;
}
