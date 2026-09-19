import { html } from '@webjsdev/core';
import '#site/components/fleet-demo.ts';
import { terminal } from '#site/lib/ui/terminal.ts';
import { section } from '#site/lib/ui/section.ts';
import { inlineFact } from '#site/lib/ui/stat.ts';
import { BTN_PRIMARY, PANEL, PROSE, LINK, BTN_GHOST, HAIRLINE } from '#site/lib/design/recipes.ts';
import { GH_URL, NEW_TAB } from '#site/lib/links.ts';
import { pageHero } from '#site/lib/ui/page-hero.ts';

/**
 * The architecture page.
 *
 * This is the site's strongest asset and the reason a skeptical infrastructure
 * reader stays. "No control plane" is a claim competitors structurally cannot
 * make, and the details behind it (a gossiped CRDT, a single-writer rule,
 * content-addressed storage) are only convincing at full specificity. So this
 * page is dense on purpose: the audience reads, and thinning it out to look
 * approachable would remove the only thing that distinguishes it.
 *
 * Everything here comes from ARCHITECTURE.md. Nothing is
 * invented for the page, which is what AGENTS.md invariant 7 is about: generic
 * copy is generic because generation averages, and the antidote is knowledge
 * that is not in the average.
 */

export const metadata = {
  title: 'Architecture - no control plane, one primitive',
  description:
    'How Pilots runs Firecracker microVMs with no scheduler tier and no managed database. Gossiped CRDT state, content-addressed snapshots, and a router that wakes machines on request.',
};

const INVARIANTS = [
  [
    'The data plane never depends on the control plane',
    'Routing and wake read local state only. There is no scheduler tier, no managed database, and no load balancer appliance. No request path may require a specific machine to be alive, and a change that breaks that is not a regression to be tuned, it is the wrong change.',
  ],
  [
    'A host writes only its own rows',
    'State is a CRDT with last-write-wins merges, which means there are no uniqueness constraints and no cross-host transactions to lean on. Two hosts writing the same row does not error. It corrupts silently, which is why this one is enforced in review rather than by the database.',
  ],
  [
    'Object storage is the only truth',
    'Local NVMe is a cache and nothing more, for a running machine’s root disk and for a volume alike. There is one storage model rather than a local disk plus a network one. The design test is blunt: wipe any host’s disk and nothing is lost. Anything that fails that test is state living in the wrong place.',
  ],
  [
    'URLs are permanent',
    'An address survives suspend, wake, checkpoint, restore, promote, redeploy, and migration to another host. Any change that can mint a new URL for an existing machine is a bug, not a tradeoff.',
  ],
  [
    'Snapshots are host-agnostic',
    'Nothing host-specific may enter a snapshot. That single requirement is why guest networking uses constant addresses and why the rootfs is bind-mounted to a constant path, both described below.',
  ],
];

/**
 * The system as five layers, in plain words.
 *
 * This is the on-ramp the page lacked. Everything below it assumes the reader
 * already holds the whole shape in their head, and most do not. The layers are
 * in the order they make sense in: each one works without the ones above it.
 * The first is the UNIT, not a count. It was once titled as one machine on one
 * box, which read as a limit on a platform where a server runs many.
 */
const LAYERS = [
  [
    'The machine',
    'A small virtual machine that boots, runs a command, and can be paused, photographed and brought back exactly as it was. One server runs many of them side by side, each inside its own private network. A router in front holds a request open and wakes the machine it is for, and a monitor puts a machine back to sleep when nothing is using it. Everything else is this, made faster, spread wider, and given a product face.',
  ],
  [
    'Making it instant',
    'A photograph of a machine is stored as small blocks, and only the blocks that differ from a shared starting point are kept. Memory and disk are then loaded on demand rather than up front, so a machine starts running before most of it has arrived. That is why creating one is a restore rather than a boot, and why saving one does not wait for the upload.',
  ],
  [
    'Many servers, and nobody in charge',
    'Every server runs the same software and can answer any request about any machine. They share what they know by gossiping it to each other, over a private encrypted network, instead of asking a central database. When a server dies the others notice on their own and bring its machines back, with the same addresses.',
  ],
  [
    'Storage that outlives the server',
    'Nothing a machine owns lives only on the server running it. Its disk and its volume are both kept in object storage, and the server’s own fast disk is a cache in front of that. A volume is saved on every write, and a machine’s disk is saved within a short, published window. Lose a server and you lose speed for a while, not data.',
  ],
  [
    'The product on top',
    'A Dockerfile, or a framework the platform recognises, becomes the same kind of image a sandbox starts from. A service is one or more machines behind an address that never changes, deployed behind a health check with a rollback. Around that sit custom domains with automatic certificates, organisations, scoped keys and quotas, a command line, typed clients, and a dashboard that runs on the platform it describes.',
  ],
];

export default function Architecture() {
  return html`
    ${pageHero({
      heading: 'No control plane, on purpose',
      lede: html`Every host runs the identical stack and serves the entire API. There is no scheduler to
        register with, no database to fail over, and no appliance in front. The tradeoffs that choice
        forces are the interesting part, and they are all below.`,
      actions: html`<a class=${BTN_PRIMARY} href="/architecture/internals">The internals, with diagrams</a>
        <a class=${BTN_GHOST} href=${GH_URL} target="_blank" rel="noopener">Read ARCHITECTURE.md${NEW_TAB}</a>`,
    })}

    ${section({
      id: 'layers',
      heading: 'The whole system, as five layers',
      lede: html`Read this first if the rest of the page is more detail than you came for. Each layer
        works without the ones above it.`,
      body: html`
        <ol class="m-0 p-0 list-none flex flex-col">
          ${LAYERS.map(
            ([title, body], i) => html`
              <li class="grid grid-cols-[2.5rem_1fr] gap-4 py-6 ${i > 0 ? 'border-t border-rule' : ''}">
                <span class="font-mono text-sm text-ink-subtle pt-1">${i + 1}</span>
                <div>
                  <h3 class="text-h3 font-semibold m-0">${title}</h3>
                  <p class="${PROSE} m-0 mt-2">${body}</p>
                </div>
              </li>
            `,
          )}
        </ol>
        <p class="${PROSE} mt-8">
          The rest of this page is what those five sentences cost to make true.
          <a class=${LINK} href="/architecture/internals">The internals page</a> draws each layer.
        </p>
      `,
    })}

    ${section({
      id: 'invariants',
      heading: 'Five things that are never traded away',
      lede: html`Several of these were paid for with production incidents in the codebase that came
        before this one. They are written down because the expensive ones are the ones that look
        optional right up until the moment they are not.`,
      body: html`
        <ol class="m-0 p-0 list-none flex flex-col">
          ${INVARIANTS.map(
            ([title, body], i) => html`
              <li class="grid grid-cols-[2.5rem_1fr] gap-4 py-6 ${i > 0 ? 'border-t border-rule' : ''}">
                <span class="font-mono text-sm text-ink-subtle pt-1">${i + 1}</span>
                <div>
                  <h3 class="text-h3 font-semibold m-0">${title}</h3>
                  <p class="${PROSE} m-0 mt-2">${body}</p>
                </div>
              </li>
            `,
          )}
        </ol>
      `,
    })}

    ${section({
      id: 'host',
      layout: 'split',
      heading: 'Three processes, and that is the whole machine',
      lede: html`A Pilots host is not a node in a cluster that something else manages. It runs
        ${inlineFact('processes')} processes, holds a full replica of the fleet’s state, and can
        answer any API call for any machine in the fleet, including ones it has never run.`,
      body: html`
        <div class="grid gap-6 mid:grid-cols-3">
          <div class="${PANEL} p-5">
            <p class="font-semibold m-0 mb-1.5">hostd</p>
            <p class="text-sm text-ink-muted m-0">
              The entire data plane in one Go binary: the API, the router and its TLS, the
              Firecracker supervisor, the block layer, the idle monitor, and the self-heal loop.
            </p>
          </div>
          <div class="${PANEL} p-5">
            <p class="font-semibold m-0 mb-1.5">corrosion</p>
            <p class="text-sm text-ink-muted m-0">
              Gossip-replicated SQLite. Every host reads its own local replica, so a lookup on the
              request path is a local read rather than a network call to something that might be
              down.
            </p>
          </div>
          <div class="${PANEL} p-5">
            <p class="font-semibold m-0 mb-1.5">firecracker</p>
            <p class="text-sm text-ink-muted m-0">
              One process per machine, inside its own network namespace, under the jailer with a
              cgroup slice bounding CPU, memory, and process count.
            </p>
          </div>
        </div>

        <hr class="${HAIRLINE} my-12" />

        <h3 class="text-h3 font-semibold m-0">Why a CRDT forces the single-writer rule</h3>
        <p class="${PROSE} mt-3">
          Gossiped state buys availability: no host waits on a quorum, and a partitioned host keeps
          serving. What it costs is arbitration. Last-write-wins means two hosts editing one row
          produce a merge rather than a conflict, and the loser vanishes without an error anywhere.
          So ownership is assigned instead of contested, and anything genuinely needing a single
          actor is resolved by hashing rather than by electing:
        </p>
        <div class="mt-6 max-w-[52ch]">
          ${terminal('deterministic ownership', [
            { kind: 'out', text: 'owner(key) = hash(key) mod live_hosts' },
            { kind: 'note', text: 'name allocation, self-heal slices, build assignment' },
          ])}
        </div>
        <p class="${PROSE} mt-6">
          Every host computes the same answer from the same inputs, so they agree without talking.
          That is the whole coordination mechanism, and it is why there is no leader to lose.
        </p>
      `,
    })}

    ${section({
      id: 'snapshots',
      heading: 'A snapshot that does not know which host made it',
      lede: html`A memory snapshot is only portable if nothing host-specific got baked into it, and
        the two places that happens are networking and file paths. Both are solved by making the
        guest’s view of the world constant, and keeping the parts that genuinely differ outside
        the snapshot.`,
      body: html`
        <div class="grid gap-6 wide:grid-cols-2">
          <div>
            <h3 class="text-h3 font-semibold m-0">Constant addresses</h3>
            <p class="${PROSE} mt-3">
              Every guest, on every host, sees the same network: the same address on the same
              interface behind the same gateway. The per-slot addressing that actually routes packets
              exists only in the namespace’s translation rules, which are rebuilt at restore and
              never enter the snapshot. Each host carries ${inlineFact('slots')} such slots.
            </p>
          </div>
          <div>
            <h3 class="text-h3 font-semibold m-0">A constant path</h3>
            <p class="${PROSE} mt-3">
              A snapshot bakes in the absolute path of its disk, and sharing one rootfs file between
              machines causes lockups after resume. So each machine gets a block device of its own,
              mounted onto the same path inside its own mount namespace. No machine holds a copy of
              the disk behind it. The path is the invariant, not what sits there, and every snapshot
              restores against a path that exists identically everywhere.
            </p>
          </div>
        </div>

        <hr class="${HAIRLINE} my-12" />

        <h3 class="text-h3 font-semibold m-0">Content-addressed, so most of it is never stored</h3>
        <p class="${PROSE} mt-3">
          Memory and disk are both chunked into ${inlineFact('block')} blocks. A block that is all
          zeroes is recorded as a gap and occupies nothing. A block identical to the template it came
          from is recorded as a pointer at the template and occupies nothing. Only genuinely
          divergent blocks are stored, which is why a checkpoint of a machine that changed little
          uploads little, and why a machine that changed nothing skips the upload entirely.
        </p>
        <p class="${PROSE} mt-4">
          Chains are exactly two levels deep, a template and one diff. A reference to a grandparent
          is rejected when the header is parsed rather than discovered later as a page that resolves
          to the wrong bytes.
        </p>
      `,
    })}

    ${section({
      id: 'storage',
      layout: 'split',
      heading: 'One storage model, and the host disk is not in it',
      lede: html`Platforms that pin a disk to a host spend years making that disk movable. Pilots
        never pins one. The machine root and the volume are both true only in the bucket, and the
        host’s NVMe is a read-through cache in front of it that can be emptied at any time.`,
      body: html`
        <div class="grid gap-6 mid:grid-cols-2">
          <div class="${PANEL} p-5">
            <p class="font-semibold m-0 mb-1.5">The machine root</p>
            <p class="text-sm text-ink-muted m-0 leading-relaxed">
              The truth of a root disk is its chunked build chain in object storage. A host serves it
              as a block device over a template every machine on that host shares, with the machine’s
              own writes kept as dirty blocks. A host with a cold cache serves the root from the bucket
              while it fills, rather than waiting for a copy. Durable as of the last checkpoint,
              suspend, or flush, and a flush runs at most ${inlineFact('rootFlushWindow')} apart.
            </p>
          </div>
          <div class="${PANEL} p-5">
            <p class="font-semibold m-0 mb-1.5">The volume</p>
            <p class="text-sm text-ink-muted m-0 leading-relaxed">
              A filesystem whose blocks live in the same bucket, with its index replicated there
              continuously. Durable per write, with write-back buffering deliberately off, which is
              why a database belongs here and not on the root. It follows its machine to whichever
              host runs it next, because it was never local to the last one.
            </p>
          </div>
        </div>

        <p class="${PROSE} mt-8">
          The two promises are stated separately on purpose. A root that promised per-write
          durability would pay an upload on every write to a scratch file, and a volume that
          promised a window would be the wrong place for a ledger. A root flush pauses the guest for
          the blocks written since the last one, budgeted at ${inlineFact('rootFlushPause')}, which is
          small and is not zero. That is the one place this design trails a copy-on-write flush that
          never stops the guest, and it is measured rather than hidden.
          <a class=${LINK} href="/architecture/internals#storage">The internals page</a> draws the
          three places a byte can be.
        </p>
      `,
    })}

    ${section({
      id: 'request',
      layout: 'split',
      heading: 'A request to a sleeping machine waits while it wakes',
      lede: html`The router lives inside the same binary that supervises the microVMs, which is what
        makes waking on demand a local operation rather than a distributed one. A request arrives for
        a machine that is suspended, and the connection is simply held open while it comes back.`,
      body: html`
        <ol class="m-0 p-0 list-none flex flex-col gap-0">
          ${[
            ['Terminate TLS', 'A wildcard certificate for platform addresses, per-domain certificates issued on demand for custom ones. Any host can answer the challenge, so issuance is not pinned anywhere.'],
            ['Resolve the name', 'Parsed from the hostname, then looked up in the local replica. Microseconds, no network.'],
            ['Route or wake', 'Running here: proxy straight into the namespace. Running elsewhere: proxy over the mesh to the host that owns it. Suspended: hold the connection, restore, then proxy.'],
            ['Record activity', 'Every request and every exec touches the machine’s last-activity stamp, which is what the idle monitor reads.'],
          ].map(
            ([title, body], i) => html`
              <li class="grid grid-cols-[2rem_1fr] gap-4 py-5 ${i > 0 ? 'border-t border-rule' : ''}">
                <span class="font-mono text-sm text-ink-subtle">${i + 1}</span>
                <div>
                  <p class="font-semibold m-0">${title}</p>
                  <p class="text-sm text-ink-muted m-0 mt-1 max-w-[70ch]">${body}</p>
                </div>
              </li>
            `,
          )}
        </ol>

        <p class="${PROSE} mt-8">
          Suspension needs both of its conditions to agree. The idle timer (${inlineFact('idle')} by default)
          has to expire <em>and</em> there has to be nothing in flight. An agent partway through a
          long build generates no HTTP traffic at all, and a timer alone would put its machine to
          sleep underneath it.
        </p>
      `,
    })}

    ${section({
      id: 'failure',
      heading: 'A dead host is noticed by everyone at once',
      lede: html`Every host writes a heartbeat. After ${inlineFact('deadHost')} of silence a host is
        presumed dead, and every survivor independently rescues the slice of its machines that hashes
        to its own index. There is no election because there is nothing to elect.`,
      body: html`
        <fleet-demo></fleet-demo>
        <p class="${PROSE} mt-8">
          The rescued machines rebuild from object storage, which is the reason this works at all:
          nothing needed from the dead host, because nothing authoritative was ever only there. That
          now covers the machines that were running as well as the ones asleep. A sleeping machine
          wakes from its snapshot, and a running one cold-boots from its last flushed disk with the
          same id, name and address, having lost at most the flush window. The
          slices tile the dead host’s machines with no overlap and no gaps, so the survivors do
          not need to agree with each other, only to run the same function.
        </p>
        <p class="${PROSE} mt-4">
          Placement can still be refused. A host is the final authority on its own capacity, so a
          rescue aimed at a full host is rejected and re-hashed rather than accepted and then failed.
          Coordinators propose. Hosts dispose.
        </p>
      `,
    })}

    ${section({
      id: 'edges',
      layout: 'split',
      heading: 'What this design costs',
      lede: html`Choosing no control plane is not free. These are the bills it comes with, and they
        are structural rather than temporary.`,
      body: html`
        <div class="grid gap-px bg-rule border border-rule rounded overflow-hidden mid:grid-cols-2">
          ${[
            ['CPU vendor pools, not one fleet', 'Memory snapshots carry raw CPUID and will not cross the Intel/AMD line, so a snapshot is portable within a vendor pool and never across one. A machine whose pool has no live host does not fail: it cold-boots from its own disk, keeping its id, its name, its URL and every byte it had written.'],
            ['No cross-host transactions', 'The state layer cannot express one. Anything needing uniqueness has to be reachable by a hash instead, and anything that cannot be is a design problem rather than a query problem.'],
            ['Silent corruption is the failure mode', 'A single-writer violation produces no error at all. It merges. Review is therefore where this gets caught.'],
            ['Every host is a security boundary', 'Since every host serves the full API, every host authenticates. Key hashes are replicated so authentication survives losing any host, including the one running the dashboard.'],
          ].map(
            ([title, body]) => html`
              <div class="bg-paper-elev p-5">
                <p class="font-semibold m-0 mb-1.5">${title}</p>
                <p class="text-sm text-ink-muted m-0">${body}</p>
              </div>
            `,
          )}
        </div>

        <p class="${PROSE} mt-10">
          If this is the kind of thing you want to argue with, the design is written down in full and
          the code is next to it. <a class=${LINK} href="/architecture/internals">The internals page</a>
          draws all of it, one mechanism at a time.
        </p>
      `,
    })}
  `;
}
