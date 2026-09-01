import { html } from '@webjsdev/core';
import '#components/lifecycle-demo.ts';
import { terminal } from '#lib/ui/terminal.ts';
import { section } from '#lib/ui/section.ts';
import { readout, inlineFact } from '#lib/ui/stat.ts';
import { PANEL, PROSE, BTN_PRIMARY, BTN_GHOST, HAIRLINE } from '#lib/design/recipes.ts';
import { WORKLOAD_APEX, GH_URL, NEW_TAB } from '#lib/links.ts';
import { pageHero } from '#lib/ui/page-hero.ts';

/**
 * The sandbox face.
 *
 * The reader here is building something that runs untrusted, freshly generated
 * code, and their real question is not "how fast" but "what happens when it
 * breaks". So the page is organised around failure and recovery rather than
 * around a feature list: checkpoint before the risky step, restore after it
 * goes wrong, and an isolation boundary that holds when the code is hostile.
 */

export const metadata = {
  title: 'Sandboxes: microVMs for AI agents',
  description:
    'Firecracker microVMs that restore instead of booting, checkpoint before every risky step, and restore in place without changing the URL or the agent token.',
};

export default function Sandboxes() {
  return html`
    ${pageHero({
      heading: 'Somewhere safe to run code nobody has read',
      lede: html`An agent writes code and needs to run it immediately, in a place where the worst outcome is
        a machine you throw away. A real virtual machine gives you that boundary. Restoring one from a
        snapshot instead of booting it is what makes the boundary affordable.`,
      actions: html`<a class=${BTN_PRIMARY} href="/deploy">Then promote it</a>
        <a class=${BTN_GHOST} href=${GH_URL} target="_blank" rel="noopener">Read the source${NEW_TAB}</a>`,
    })}

    ${section({
      id: 'speed',
      heading: 'A machine that was never off',
      lede: html`Booting a Linux guest costs whatever the operating system costs, every time, which
        is why sandbox products either make you wait or keep idle machines burning money. pilots
        restores a memory snapshot of an already-running guest and pages memory in as the guest
        touches it.`,
      body: html`
        <div class="grid gap-8 mid:grid-cols-3 mid:gap-6">
          ${readout('create')} ${readout('wake')} ${readout('checkpoint')}
        </div>
        <p class="${PROSE} mt-10">
          These are the thresholds Phase 3 had to clear before it could close, timed on a laptop.
          Every number on this site carries its source. Hover one to see it. Fleet numbers replace
          these when there is a fleet to measure.
        </p>
      `,
    })}

    ${section({
      id: 'checkpoints',
      layout: 'split',
      heading: 'Checkpoint before the risky step, restore after it',
      lede: html`An agent about to run a migration, an upgrade, or a command it just invented is one
        step away from a machine that no longer works. A checkpoint makes that step cheap to undo,
        and the restore puts the state back without putting a new machine in its place.`,
      body: html`
        <div class="grid gap-8 wide:grid-cols-[1fr_1fr] wide:items-start">
          <div>
            ${terminal('checkpoint, break it, come back', [
              { kind: 'cmd', text: 'pilot checkpoint bold-otter --name pre-upgrade' },
              { kind: 'out', text: 'checkpoint pre-upgrade' },
              { kind: 'cmd', text: 'pilot exec bold-otter -- ./upgrade.sh' },
              { kind: 'out', text: 'Segmentation fault' },
              { kind: 'cmd', text: 'pilot restore bold-otter pre-upgrade' },
              { kind: 'mark', text: 'restored in place' },
              { kind: 'note', text: 'same machine, same URL, same agent token' },
            ])}
          </div>
          <div class="flex flex-col gap-5">
            <div>
              <p class="font-semibold m-0 mb-1.5">In place, on the same machine</p>
              <p class="text-sm text-ink-muted m-0">
                Restoring does not spawn a fresh machine from a template and hand you a new address.
                It is the same row in the database, the same URL, and the same credential the agent
                is already holding. Anything else would force every client to re-discover where its
                work went.
              </p>
            </div>
            <div>
              <p class="font-semibold m-0 mb-1.5">Named and chained</p>
              <p class="text-sm text-ink-muted m-0">
                Checkpoints have names and can be taken from a restored state, so an agent can walk
                back to a known-good point and try a different branch.
              </p>
            </div>
            <div>
              <p class="font-semibold m-0 mb-1.5">Durable in the background</p>
              <p class="text-sm text-ink-muted m-0">
                The machine resumes as soon as the copy-on-write layer is cloned, and the upload happens
                behind it. Durability is reported separately, so a client that genuinely needs the
                bytes in object storage can wait for exactly that.
              </p>
            </div>
          </div>
        </div>
      `,
    })}

    ${section({
      id: 'exec',
      heading: 'Streaming exec, because agents produce output for minutes',
      lede: html`Running a command and collecting its output at the end is fine for a script and
        useless for a model that emits tokens for several minutes. Both shapes exist: buffered when
        you want a result, streamed over a socket when you want to watch.`,
      body: html`
        <div class="grid gap-6 mid:grid-cols-2">
          <div class="${PANEL} p-5">
            <p class="font-semibold m-0 mb-1.5">Working directory and environment</p>
            <p class="text-sm text-ink-muted m-0">
              Every exec takes a directory, an environment, and a user, in both the buffered and the
              streaming form. A tool that only accepts them in one of the two forces its callers to
              pick between watching the output and running in the right place.
            </p>
          </div>
          <div class="${PANEL} p-5">
            <p class="font-semibold m-0 mb-1.5">Optional stdin</p>
            <p class="text-sm text-ink-muted m-0">
              A stream can be opened with no input side at all, which is what a long-running agent
              process wants: it never reads, and holding a half-open pipe for it is a way to lose the
              connection.
            </p>
          </div>
        </div>

        <p class="${PROSE} mt-8">
          Exec counts as activity. A machine running a build with no HTTP traffic at all is not idle,
          and the idle monitor knows that, which is the difference between a
          ${inlineFact('idle')} timer that is useful and one that suspends an agent mid-task.
        </p>
      `,
    })}

    ${section({
      id: 'isolation',
      layout: 'split',
      heading: 'The boundary is a whole virtual machine',
      lede: html`Containers share a kernel, so a kernel bug is a tenant boundary failure. These are
        Firecracker microVMs with their own kernels, which is the isolation model the code you are
        about to run has not read.`,
      body: html`
        <div class="grid gap-px bg-rule border border-rule rounded overflow-hidden mid:grid-cols-2">
          ${[
            ['Jailed and bounded', 'Each microVM runs under the jailer inside a cgroup slice that caps CPU, memory, and process count. A fork bomb inside is a fork bomb inside.'],
            ['Egress firewalled', 'Traffic to private ranges, loopback, and link-local is dropped inside the guest’s own namespace, so a sandbox cannot reach the host or its neighbours.'],
            ['Per-machine credentials', 'The token the guest agent accepts is minted per machine and never reused, so a leaked token is scoped to the machine that leaked it.'],
            ['Disposable by design', 'The expected end state of a sandbox is destruction. Nothing about the platform assumes a machine is precious.'],
          ].map(
            ([t, b]) => html`
              <div class="bg-paper-elev p-5">
                <p class="font-semibold m-0 mb-1.5">${t}</p>
                <p class="text-sm text-ink-muted m-0">${b}</p>
              </div>
            `,
          )}
        </div>
      `,
    })}

    ${section({
      id: 'lifecycle',
      heading: 'One address, whatever happens to the machine behind it',
      lede: html`A sandbox that changes address when it sleeps is a sandbox every client has to poll
        for. Drive one through its lifecycle and watch the address hold.`,
      body: html`
        <lifecycle-demo></lifecycle-demo>
        <p class="${PROSE} mt-8">
          Arbitrary ports are reachable too, at a prefixed name on the same address, so a dev server
          the agent started on some port is browsable without any tunnel to set up.
        </p>
      `,
    })}

    <!-- No bordered panel here. The home page closes with one, and repeating
         the same box on every page is the closing-CTA template that makes a
         site read as generated. A hairline and a wide statement end this page
         on a different shape. -->
    <div class="max-w-6xl mx-auto px-6 pb-24">
      <hr class="${HAIRLINE} mb-10" />
      <div class="grid gap-6 wide:grid-cols-[1.3fr_1fr] wide:gap-14 wide:items-end">
        <h2 class="text-h2 font-bold m-0 max-w-[20ch]">The sandbox becomes the service</h2>
        <div>
          <p class="${PROSE} m-0">
            When something an agent built turns out to matter, it does not get rebuilt somewhere
            else. It gets promoted, keeps its address at
            <span class="font-mono">${WORKLOAD_APEX}</span>, and starts being health-checked.
          </p>
          <div class="flex flex-wrap gap-3 mt-6">
            <a class=${BTN_PRIMARY} href="/deploy">How deploying works</a>
            <a class=${BTN_GHOST} href="/roadmap">Where it actually is</a>
          </div>
        </div>
      </div>
    </div>
  `;
}
