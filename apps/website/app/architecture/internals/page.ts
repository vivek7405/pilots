import { html } from '@webjsdev/core';
import { section } from '#lib/ui/section.ts';
import { pageHero } from '#lib/ui/page-hero.ts';
import { terminal } from '#lib/ui/terminal.ts';
import { inlineFact, readout } from '#lib/ui/stat.ts';
import { arrowDefs } from '#lib/ui/diagram.ts';
import { plainly } from '#lib/ui/plainly.ts';
import { PROSE, LINK, BTN_GHOST, HAIRLINE, FIELD_LABEL, PANEL } from '#lib/design/recipes.ts';
import { GH_URL, GH_BOARD_URL, NEW_TAB } from '#lib/links.ts';
import { fleetFigure, splitBrainFigure } from '#modules/internals/diagrams/fleet.ts';
import { hostFigure } from '#modules/internals/diagrams/host.ts';
import { requestFigure } from '#modules/internals/diagrams/request.ts';
import { snapshotFormatFigure, checkpointTimelineFigure } from '#modules/internals/diagrams/storage.ts';
import { lazyFigure } from '#modules/internals/diagrams/lazy.ts';
import { networkFigure } from '#modules/internals/diagrams/network.ts';
import { pipelineFigure } from '#modules/internals/diagrams/pipeline.ts';

/**
 * The internals page: the whole system, drawn.
 *
 * /architecture answers "why is this shaped like this" for a reader deciding
 * whether to keep reading. This page answers "how does it actually work" for
 * one who already decided, and the split exists because those two readers want
 * opposite things. Collapsing them produces a page that is too shallow for the
 * engineer and too long for everyone else, which is what the single page was
 * becoming.
 *
 * Everything here is drawn from ARCHITECTURE.md and the phase issues #2 to #7.
 * Nothing is invented to fill a figure. Where a mechanism is not built yet the
 * page says which phase owns it rather than describing it in the present tense,
 * because a roadmap written in the present tense is the thing this project's
 * own roadmap page exists to avoid.
 *
 * On the figures: they are hand-authored inline SVG through #lib/ui/diagram.ts,
 * which carries the reasoning for that choice. The one rule worth repeating
 * here is that text inside an <svg> is text a reader sees, so it obeys the same
 * no-slop gate as a paragraph. Measurements therefore live in the captions,
 * where the sourced-facts registry can render them.
 */

export const metadata = {
  title: 'Internals: the whole pilots architecture, drawn',
  description:
    'A technical walkthrough of pilots end to end: the fleet, one host, the CRDT state layer, the request path, content-addressed snapshots, lazy memory and disk, guest networking, and the build pipeline.',
};

/**
 * The contents strip.
 *
 * Long technical pages are read by jumping, not by scrolling, and a reader who
 * arrives from a search result wants to know in one glance whether the thing
 * they came for is here. This is a plain wrapped row of anchors rather than a
 * sticky sidebar: no script, no scroll listener, no layout that collapses on a
 * narrow viewport.
 */
const CONTENTS: [string, string][] = [
  ['plain', 'Start here'],
  ['map', 'The fleet'],
  ['host', 'One host'],
  ['state', 'State'],
  ['request', 'A request'],
  ['storage', 'Snapshots'],
  ['lazy', 'Lazy paging'],
  ['network', 'Networking'],
  ['pipeline', 'Build and deploy'],
  ['surface', 'The surface'],
  ['phases', 'Phase by phase'],
  ['glossary', 'Glossary'],
  ['numbers', 'Numbers'],
];

/**
 * The glossary.
 *
 * A technical page loses non-specialist readers on vocabulary long before it
 * loses them on ideas, and the words below are the ones doing that work here.
 * Each definition is written to be true rather than merely reassuring: where
 * the short version omits something load-bearing, the omission is named.
 */
const GLOSSARY: [string, unknown][] = [
  [
    'microVM',
    'A whole computer, simulated in software, with its own kernel and its own memory. Heavier than a container, which shares the host kernel with its neighbours, and much lighter than an ordinary virtual machine. The isolation is real hardware-assisted isolation, which is why untrusted code can be run in one.',
  ],
  [
    'snapshot',
    'A byte-for-byte copy of a running machine, its memory included. Restoring one does not start the machine, it continues it: the programs inside were mid-sentence and pick up mid-sentence. This is why starting a sandbox is fast and why a suspended machine costs nothing while asleep.',
  ],
  [
    'control plane',
    'The coordinating brain a platform usually has: a scheduler deciding where things run, a central database holding what is true, a load balancer at the front. pilots has none of the three, which is the claim the rest of this page is spent paying for.',
  ],
  [
    'gossip',
    'Instead of asking a central database, every machine keeps its own full copy of the shared facts and continuously tells its neighbours about changes. Reading is instant and local. The price is that two machines can briefly hold different answers.',
  ],
  [
    'a CRDT, and last-write-wins',
    'The rule for combining two copies of a record that were edited independently. Here the later edit wins, field by field. Nothing errors when two hosts edit the same record, which sounds convenient and is actually the most dangerous property in the system.',
  ],
  [
    'content-addressed storage',
    'Data is stored in fixed-size pieces, and a piece that already exists is referred to rather than stored again. A machine that changed almost nothing therefore uploads almost nothing, because most of its pieces are still the ones it started with.',
  ],
  [
    'a page fault',
    'What happens when a program reaches for memory that is not actually loaded. The processor pauses that program, someone supplies the missing piece, and it carries on with no idea anything happened. pilots uses this to start a machine before its memory has finished arriving.',
  ],
  [
    'a namespace',
    'A private view of part of the system, given to one process. A machine here gets its own network and its own filesystem view, so it can believe it has an address and a disk path that every other machine also believes it has.',
  ],
];

/** The state tables, and who is allowed to write each one. */
const TABLES: [string, string, string][] = [
  ['machines', 'the host running it', 'Everything about one microVM: its name, its owner, its lifecycle knobs, the builds it was last captured into, and the moment it was last touched.'],
  ['hosts', 'the host itself', 'Free capacity and a heartbeat. The heartbeat is what the self-heal loop reads, and the capacity is advisory, because a host is the final authority on whether it accepts a placement.'],
  ['checkpoints', 'the host running the machine', 'Named, sequenced, and pointing at the two builds that reconstitute the machine at that moment.'],
  ['services', 'the host running it', 'The production face: replicas, the current release, a health specification, plain environment values, and sealed ones.'],
  ['releases', 'the host running the service', 'One rootfs build plus whether it was ever observed healthy, which is what a rollback selects against.'],
  ['volumes', 'the host it is mounted on', 'Where a volume lives in object storage, and which machine currently has it.'],
  ['api_keys', 'any host, on an admin-scoped request', 'Key hashes only, replicated everywhere so that every host authenticates against its own disk. Each row is written once, which is what makes any host a safe writer.'],
  ['api_key_revocations', 'any host, on an admin-scoped request', 'A tombstone per revoked key. It only ever appears and never changes, so no two writers can disagree about it.'],
  ['tenancy', 'the host writing the object row', 'Which org owns each machine, service, and volume. Written once, before the object row it names, so a create that dies partway leaves an owner and never an orphan.'],
  ['org_quotas', 'any host, on an admin-scoped request', 'Ceilings per org on machines, cores, memory, volume space, and concurrent builds. One logical writer per row, so the merge has nothing to corrupt.'],
];

export default function Internals() {
  return html`
    ${arrowDefs()}

    ${pageHero({
      heading: 'The whole thing, drawn',
      lede: html`Nine figures and the mechanisms behind them: how a fleet with no coordinator agrees,
        what a request actually does, why a snapshot restores on a machine that never made it, and
        where the freeze in a checkpoint really is. Every section also states its idea in ordinary
        words, so the page is readable without the background it otherwise assumes.`,
      actions: html`<a class=${BTN_GHOST} href="${GH_URL}/blob/main/ARCHITECTURE.md" target="_blank" rel="noopener"
          >ARCHITECTURE.md${NEW_TAB}</a
        >
        <a class=${BTN_GHOST} href="/architecture">The shorter version</a>`,
    })}

    <nav aria-label="On this page" class="border-b border-rule">
      <div class="max-w-6xl mx-auto px-6 py-4 flex flex-wrap items-baseline gap-x-5 gap-y-2">
        <span class="${FIELD_LABEL}">On this page</span>
        ${CONTENTS.map(
          ([id, label]) => html`
            <a class="text-sm text-ink-muted no-underline hover:text-ink transition-colors" href="#${id}"
              >${label}</a
            >
          `,
        )}
      </div>
    </nav>

    ${section({
      id: 'plain',
      layout: 'split',
      heading: 'The whole idea, before any of the detail',
      lede: html`The rest of this page is written for somebody who already knows what a page fault is.
        This section is not. Read it and you can follow every figure below, and each section repeats
        its own idea in ordinary words underneath the technical version.`,
      body: html`
        <div class="grid gap-10 wide:grid-cols-[1.05fr_0.95fr]">
          <div>
            <p class="${PROSE} m-0">
              pilots runs your code inside a tiny simulated computer. Not a shared container with a
              fence around it, an actual separate machine with its own kernel, which is what makes it
              safe to hand one to a stranger or to an AI agent that is about to run something reckless.
              The trick is that starting one does not mean booting one. The system keeps a photograph
              of a machine that has already finished starting up, and a new machine is that photograph
              brought back to life. Nothing boots, so nobody waits the ${inlineFact('settle')} that
              booting and settling actually takes.
            </p>
            <p class="${PROSE} mt-4">
              The same photograph is how a machine goes to sleep and comes back. Stop touching it and it
              is captured and put away, costing nothing. Send it a request and the request waits, for
              about as long as a slow web page takes to load, while the machine is brought back exactly
              where it left off. Its address never changes through any of this, which is the part
              everything else is arranged around.
            </p>
            <p class="${PROSE} mt-4">
              The unusual decision is that there is no head office. Most platforms have one place that
              decides where things run and one database that knows what is true, and if that place is
              having a bad day, nothing works. Here every server runs the identical software, keeps its
              own complete copy of the shared facts, and can answer any question about any machine in
              the system. They tell each other about changes constantly, the way a rumour spreads, so
              nobody has to ask permission to act.
            </p>
          </div>
          <div>
            <div class="${PANEL} p-6">
              <p class="font-semibold m-0">Why anyone would build it this way</p>
              <p class="text-sm text-ink-muted m-0 mt-2">
                A head office is a single thing that can be down. Removing it means no request ever
                depends on a particular server being alive, and a server that loses contact with the
                others keeps working rather than freezing. It also means adding capacity is handing the
                system an IP address, because there is nothing to register with.
              </p>
              <hr class="${HAIRLINE} my-5" />
              <p class="font-semibold m-0">What it costs</p>
              <p class="text-sm text-ink-muted m-0 mt-2">
                Without one authority, two servers can briefly believe different things and neither is
                told it is wrong. There are no guarantees of the kind a normal database hands out for
                free. Most of the awkward machinery on this page is the price of that, paid in advance
                and on purpose.
              </p>
              <hr class="${HAIRLINE} my-5" />
              <p class="font-semibold m-0">Where the real copy lives</p>
              <p class="text-sm text-ink-muted m-0 mt-2">
                In shared object storage, not on any one server's disk. The local disk is only a cache.
                The design is tested against a blunt question: wipe any server completely, and has
                anything been lost. The answer has to be no.
              </p>
            </div>
          </div>
        </div>
      `,
    })}

    ${section({
      id: 'map',
      heading: 'What a fleet is, and what it is missing',
      lede: html`Adding capacity is running one script against an IP address. There is nothing for the
        new host to register with, because the thing it would register with does not exist.`,
      body: html`
        ${fleetFigure()}
        <div class="grid gap-8 mt-12 wide:grid-cols-2">
          <div class="min-w-0">
            <h3 class="text-h3 font-semibold m-0">The three that are not there</h3>
            <p class="${PROSE} mt-3">
              A platform this shape normally has a scheduler tier deciding placement, a managed database
              holding the truth, and a load balancer in front terminating connections. Each is a thing
              that can be down while every host is up. Removing all three is not an optimisation, it is
              the constraint the rest of the design is bent around, and most of the awkwardness on this
              page is the bill for it.
            </p>
          </div>
          <div class="min-w-0">
            <h3 class="text-h3 font-semibold m-0">Coordination without a coordinator</h3>
            <p class="${PROSE} mt-3">
              Where something genuinely needs a single actor, ownership is computed rather than elected.
              Every host runs the same function over the same replicated inputs and reaches the same
              answer without exchanging a message about it. That covers name allocation, which host
              builds an image, and which survivor rescues which machines.
            </p>
            <div class="mt-5 max-w-[46ch]">
              ${terminal('deterministic ownership', [
                { kind: 'out', text: 'owner(key) = hash(key) mod live_hosts' },
                { kind: 'note', text: 'sorted identically on every host, or it does not tile' },
              ])}
            </div>
          </div>
        </div>
        ${plainly(html`Picture a team where nobody is in charge, but everyone carries the same rulebook
          and the same list of who is on shift. When a job needs exactly one owner, each of them applies
          the rule to the job's name, and they all get the same answer without a meeting. Take somebody
          off the list and everyone recalculates, still without a meeting. That is the entire management
          structure.`)}
      `,
    })}

    ${section({
      id: 'host',
      layout: 'split',
      heading: 'One host, and the whole data plane is on it',
      lede: html`A host runs ${inlineFact('processes')} processes. One of them is the entire product:
        the API, the router, the microVM supervisor, the storage client, and the self-heal loop are
        packages in a single Go binary, and that is why waking a sleeping machine is a function call.`,
      body: html`
        ${hostFigure()}

        <hr class="${HAIRLINE} my-12" />

        <div class="grid gap-10 wide:grid-cols-[1fr_1fr]">
          <div class="min-w-0">
            <h3 class="text-h3 font-semibold m-0">Every host serves this</h3>
            <p class="${PROSE} mt-3">
              There is no separate admin API and no host that answers more than another. A request for a
              machine on the other side of the fleet is served by whichever host received it, which is
              also what makes the wildcard DNS record legitimate rather than a trick.
            </p>
          </div>
          <div class="min-w-0">
            ${terminal('the public surface, on every host', [
              { kind: 'out', text: 'POST   /v1/machines' },
              { kind: 'out', text: 'POST   /v1/machines/:id/exec' },
              { kind: 'out', text: 'GET    /v1/machines/:id/exec/stream' },
              { kind: 'out', text: 'POST   /v1/machines/:id/checkpoints' },
              { kind: 'out', text: 'POST   /v1/checkpoints/:id/restore' },
              { kind: 'out', text: 'POST   /v1/machines/:id/suspend | wake' },
              { kind: 'out', text: 'POST   /v1/builds' },
              { kind: 'out', text: 'POST   /v1/services/:id/deploy | rollback' },
              { kind: 'out', text: 'POST   /v1/machines/:id/promote' },
              { kind: 'out', text: 'GET    /v1/hosts' },
              { kind: 'note', text: 'bearer auth, checked against the local replica' },
            ])}
          </div>
        </div>
        ${plainly(html`Most systems split the front desk from the back room, so a visitor's request has
          to be passed between them. Here they are the same room. The part that answers the phone and
          the part that runs the machines are the same program on the same computer, so waking a
          sleeping machine to answer a call is not a negotiation between departments. It is one program
          doing two things it can already do.`)}
      `,
    })}

    ${section({
      id: 'state',
      heading: 'State that merges instead of failing',
      lede: html`Fleet state is a set of CRDT tables gossiped between hosts, so every lookup on the
        request path is a read against a local disk. What that buys is availability. What it costs is
        every guarantee a database normally provides, and the table below is how that cost is paid.`,
      body: html`
        <div class="overflow-x-auto scroll-thin">
          <table class="w-full border-collapse text-sm min-w-[640px]">
            <caption class="sr-only">
              Each replicated table, the only host permitted to write it, and what it holds
            </caption>
            <thead>
              <tr class="border-b border-rule-strong text-left">
                <th scope="col" class="py-3 pr-6 font-mono text-xs uppercase tracking-[0.14em] text-ink-subtle font-medium">Table</th>
                <th scope="col" class="py-3 pr-6 font-mono text-xs uppercase tracking-[0.14em] text-ink-subtle font-medium">Writer</th>
                <th scope="col" class="py-3 font-mono text-xs uppercase tracking-[0.14em] text-ink-subtle font-medium">What it holds</th>
              </tr>
            </thead>
            <tbody>
              ${TABLES.map(
                ([name, writer, holds]) => html`
                  <tr class="border-b border-rule align-top">
                    <td class="py-4 pr-6 font-mono text-[13px] whitespace-nowrap">${name}</td>
                    <td class="py-4 pr-6 text-ink-muted whitespace-nowrap">${writer}</td>
                    <td class="py-4 text-ink-muted">${holds}</td>
                  </tr>
                `,
              )}
            </tbody>
          </table>
        </div>

        <p class="${PROSE} mt-10">
          Nothing enforces that column. Two hosts writing one row does not conflict and does not error,
          it merges, and the loser disappears with no trace anywhere. That is why the writer is a
          property of the design rather than a constraint in a schema, and why the exceptions are
          enumerated rather than left to judgement.
        </p>

        <div class="mt-12">${splitBrainFigure()}</div>

        <div class="grid gap-px bg-rule border border-rule rounded overflow-hidden mt-12 mid:grid-cols-3">
          ${[
            ['Rows are never deleted', 'A delete racing an update can resurrect the row through the merge, so a destroyed machine is marked destroyed and collected later by a reaper. The same reasoning applies to revoking an API key.'],
            ['Names are not unique', 'There is no uniqueness constraint to lean on, so during a membership change two hosts can briefly both believe they own a name. The router resolves a duplicate by taking the lowest machine id and logging it loudly.'],
            ['Schema changes are a fleet operation', 'Altering a live table makes the CRDT layer backfill every row, which is a gossip storm rather than a migration. Evolution means a new table and a period of reading both.'],
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
          Gossip travels as QUIC inside the encrypted mesh, with the datagram size pinned to
          ${inlineFact('gossipMtu')} bytes: the smallest the underlay can be, rather than the largest
          this host happens to support. Left to discover its own path size, it overestimates across a
          mixed underlay and drops gossip in a way that looks exactly like a cluster flapping at random.
        </p>
        ${plainly(html`Everyone keeps their own copy of the shared notebook and shouts out their edits.
          Nobody has to ask a central office anything, which is why the system stays up when parts of it
          do not. The catch is that if two people write on the same line, the notebook does not object.
          It quietly keeps the later one and the other is simply gone. So the fix is a discipline rather
          than a lock: each line has exactly one person allowed to write it, and if you find yourself
          holding something the notebook says belongs to somebody else, you put it down.`)}
      `,
    })}

    ${section({
      id: 'request',
      layout: 'split',
      heading: 'One request, three endings',
      lede: html`The router is a package in the binary that supervises the microVMs, so the branch where
        the machine is asleep is not a distributed operation. The connection is held, the machine is
        restored underneath it, and the response arrives late rather than never.`,
      body: html`
        ${requestFigure()}
        ${plainly(html`A request arrives and the server looks up, on its own disk, where that machine
          is. Three things can be true. It is here, so the request goes straight in. It is on a
          colleague's server, so the request is forwarded. Or it is asleep, in which case nobody sends
          back an error or a "please wait" page. The browser just keeps waiting, the machine is brought
          back, and the answer arrives slightly late. Putting a machine to sleep in the first place
          needs two things to be true at once, because an AI agent halfway through a long job sends no
          web traffic at all, and a system watching only for traffic would switch it off underneath
          itself.`)}
      `,
    })}

    ${section({
      id: 'storage',
      heading: 'A snapshot that does not know which host made it',
      lede: html`Machine state lives in object storage as content-addressed blocks, and local disk is a
        cache of it. The design test is blunt. Wipe any host's disk and nothing is lost.`,
      body: html`
        ${snapshotFormatFigure()}

        <div class="grid gap-8 mt-12 wide:grid-cols-2">
          <div>
            <h3 class="text-h3 font-semibold m-0">A machine is pinned to its template</h3>
            <p class="${PROSE} mt-3">
              A diff's unchanged ranges name a logical offset rather than bytes, so they mean nothing
              except against the exact build they were encoded against. The machine row therefore records
              which template it was created from, and every later capture and restore uses that one, never
              whichever template the acting host happens to hold. The two differ routinely. A host
              restoring a machine whose template it lacks downloads it, which is possible precisely
              because builds are content-addressed.
            </p>
          </div>
          <div>
            <h3 class="text-h3 font-semibold m-0">The filesystem underneath matters</h3>
            <p class="${PROSE} mt-3">
              Create copies the golden template and checkpoint copies the snapshot inside the pause
              window, and both are budgeted as metadata operations. That is only true on a filesystem
              that can share extents. On one that cannot, nothing errors: the copy silently becomes a
              real copy, ${inlineFact('rootfsCopy')} of it, and create measures
              ${inlineFact('ext4Create')} against a ${inlineFact('create')} budget with no explanation
              anywhere. Hosts now probe for this at startup, report it on the health endpoint, and the
              bootstrap script refuses to finish without it when asked to.
            </p>
          </div>
        </div>

        <hr class="${HAIRLINE} my-12" />

        ${checkpointTimelineFigure()}

        <p class="${PROSE} mt-10">
          Durability is two separate signals, and collapsing them would be expensive. One says the builds
          exist on this host, which is everything a local rollback needs. The other says they are
          uploaded, which is what a restore anywhere else needs. A single flag would make every rollback
          wait for an upload it is never going to read.
        </p>
        ${plainly(html`A machine is stored the way a photo album stores a burst of near-identical
          photographs. The first one is kept in full. Every one after it is kept as the handful of
          details that differ, plus a note saying the rest is unchanged. A machine that sat idle
          therefore takes almost no space and almost no time to save, and one that never wrote to its
          disk skips being saved at all. The second figure is about a different confusion. Saving a
          machine takes a while, but the machine is only frozen for the middle part of it, so timing
          the whole operation from outside makes the pause look several times worse than anyone inside
          it actually experienced.`)}
      `,
    })}

    ${section({
      id: 'lazy',
      layout: 'split',
      heading: 'The guest starts before its memory arrives',
      lede: html`A restore does not wait for a memory image or a disk image to land. It starts the guest
        and answers the faults, which is the difference between a wake measured in milliseconds and one
        measured in the size of the machine.`,
      body: html`
        ${lazyFigure()}
        ${plainly(html`Waiting for a whole machine's memory to download before letting it run would make
          waking one slow and would make it slower the bigger the machine is. So the machine is started
          empty and allowed to ask. Whenever it reaches for something that has not arrived, it is
          paused for an instant, the missing piece is fetched, and it carries on without ever knowing.
          Most of what it reaches for is fetched in one go rather than piece by piece, which is the
          difference between this working and it being unusably slow. The system also writes down the
          order things were asked for, so next time it can fetch them before they are wanted.`)}

        <hr class="${HAIRLINE} my-12" />

        <h3 class="text-h3 font-semibold m-0">Four things the kernel does not forgive</h3>
        <p class="${PROSE} mt-3">
          Both handlers are ports rather than rewrites, because what they encode is kernel behaviour that
          is expensive to rediscover. These four cost the most to learn.
        </p>
        <ol class="m-0 mt-6 p-0 list-none flex flex-col">
          ${[
            [
              'A signal delivered to the wrong thread kills a restore',
              'The block handler parks in a kernel call for the life of the device, and any signal delivered to that thread makes the kernel tear the device down. The Go runtime preempts goroutines with a signal, so roughly one restore in four died. The symptom is thoroughly misleading: the attach succeeds, the kernel logs a capacity change, the size returns to zero, and the caller times out against a device that reports no owner and looks free.',
            ],
            [
              'A disconnected device wedges the host, not the machine',
              'A handler blocked in that same call never reaches its own cleanup, so the device has to be disconnected by the parent before the handler is killed. Get the order wrong and the microVM process blocks uninterruptibly with a dead device until the host reboots.',
            ],
            [
              'An empty diff is an empty object, and a range read of it fails',
              'A machine that wrote nothing produces a zero-length data object, and a ranged read against it returns a not-satisfiable status. Treating that as an error kills the wake. It means zeros, and the cache is marked accordingly.',
            ],
            [
              'Retrying forever is worse than failing',
              'A page copy that returns a retryable error is retried a bounded number of times, because an unbounded retry turns a persistent one into a worker spinning on a core with a guest thread blocked behind it.',
            ],
          ].map(
            ([title, body], i) => html`
              <li class="grid grid-cols-[2rem_1fr] gap-4 py-5 ${i > 0 ? 'border-t border-rule' : ''}">
                <span class="font-mono text-sm text-ink-subtle">${i + 1}</span>
                <div>
                  <p class="font-semibold m-0">${title}</p>
                  <p class="text-sm text-ink-muted m-0 mt-1 max-w-[74ch]">${body}</p>
                </div>
              </li>
            `,
          )}
        </ol>
      `,
    })}

    ${section({
      id: 'network',
      heading: 'Every guest has the same address, deliberately',
      lede: html`Nothing host-specific may enter a snapshot, and the two places it otherwise would are
        networking and file paths. Both are solved by making the guest's view constant and keeping
        everything that genuinely differs outside the image.`,
      body: html`
        ${networkFigure()}

        <p class="${PROSE} mt-10">
          Two consequences worth stating plainly. A machine rescued onto another host takes a new slot,
          so its peer address changes, which is why name answers carry a near-zero lifetime and why a
          connection pool holding an open socket to the old address simply breaks. Recovery covers the
          platform, not an application's own connections. And because addresses are derived from a host's
          key, rotating that key readdresses every machine on it, so the host is drained first or every
          one of them takes a reset.
        </p>
        ${plainly(html`Every machine is told it lives at the same address as every other machine. That
          sounds broken and is the point: a photograph of a machine can only be brought back somewhere
          else if nothing inside it refers to where it used to be. The machines cannot talk to each
          other directly, so the host translates on their behalf, the way an old telephone exchange
          connected two callers who only ever knew their own number. One real consequence for anyone
          building on it: when a machine is rescued onto a different server it effectively gets a new
          phone number, so an application holding an open connection to the old one will see it drop and
          has to dial again.`)}
      `,
    })}

    ${section({
      id: 'pipeline',
      layout: 'split',
      heading: 'A Dockerfile becomes the same thing a sandbox starts from',
      lede: html`The production face is not a second system. A build produces an ordinary
        content-addressed template, a release points at one, and starting a replica is the restore path
        this page has already described.`,
      body: html`
        ${pipelineFigure()}

        <hr class="${HAIRLINE} my-12" />

        <h3 class="text-h3 font-semibold m-0">Three ways in, one pipeline</h3>
        <div class="grid gap-6 mt-6 mid:grid-cols-3">
          <div class="${PANEL} p-5">
            <p class="font-semibold m-0 mb-1.5">Direct</p>
            <p class="text-sm text-ink-muted m-0">
              The command line tars the local context and posts it. No repository host is involved, which
              is what the SDKs and the agent flow both use underneath.
            </p>
          </div>
          <div class="${PANEL} p-5">
            <p class="font-semibold m-0 mb-1.5">Connected to a repository</p>
            <p class="text-sm text-ink-muted m-0">
              The webhook endpoint is another route on every host, so any host can receive it and a hash
              of the repository name picks the builder. There is no continuous integration service in
              the middle.
            </p>
          </div>
          <div class="${PANEL} p-5">
            <p class="font-semibold m-0 mb-1.5">A sandbox per pull request</p>
            <p class="text-sm text-ink-muted m-0">
              A preview is a sandbox rather than a service, so it idle-suspends to roughly nothing
              between visits and is destroyed when the request closes.
            </p>
          </div>
        </div>

        <p class="${PROSE} mt-10">
          Build logs are structured rather than a text stream, and that is a product decision. An agent
          pointed at a repository with no Dockerfile writes one, reads the failing step out of the
          stream when it is wrong, patches it, and goes again. The loop is what the structure exists
          for, and it is the flow the final phase gates on.
        </p>
        ${plainly(html`A sandbox and a production website are the same object here, with different
          settings. Deploying an app turns your code into exactly the kind of photograph a sandbox
          starts from, so launching a second copy of a live service is the same instant restore, not a
          cold start. Before the new version replaces the old one, the platform checks that it actually
          answers. If it does not, the old version was never thrown away, and traffic stays on it.`)}
      `,
    })}

    ${section({
      id: 'surface',
      heading: 'What drives all of this',
      lede: html`The engine finished ahead of the surface over it. What exists on the API today is
        tenancy, scoped keys, revocation, and quotas. The dashboard and the command line are in review,
        and the tool server for agents ships inside the command line.`,
      body: html`
        <div class="grid gap-10 wide:grid-cols-[0.9fr_1.1fr]">
          <div class="min-w-0">
            <h3 class="text-h3 font-semibold m-0">Authentication survives losing any host</h3>
            <p class="${PROSE} mt-3">
              A key is minted through the API on any host, under the admin scope, and the first one
              comes from an operator on a host. The dashboard calls that route and never verifies a key.
              From then on every host authenticates against its own disk, so killing the dashboard's
              host changes nothing about which keys the API accepts. Revocation writes a tombstone
              rather than deleting a row, for the resurrection reason above.
            </p>
            <p class="${PROSE} mt-4">
              An agent is a first-class principal here. An API key is all one needs, scopes on the key
              bound what it can do, and the tool server for agents reads the same credentials file the
              command line does.
            </p>
          </div>
          <div class="min-w-0">
            ${terminal('the guest agent, inside every machine', [
              { kind: 'out', text: 'GET  /health' },
              { kind: 'out', text: 'POST /init          set the wall clock after a restore' },
              { kind: 'out', text: 'POST /exec          buffered, with cwd, env and user' },
              { kind: 'out', text: 'GET  /exec/stream   binary frames: stdout, stderr, exit' },
              { kind: 'out', text: 'GET  /terminal      a pty over a socket' },
              { kind: 'mark', text: 'X-Pilot-Proxy-Port  reach any port the app listens on' },
              { kind: 'note', text: 'restoring a snapshot leaves the clock frozen, so /init is not optional' },
            ])}
            <p class="text-sm text-ink-muted mt-4 max-w-[62ch]">
              The frame protocol is byte-compatible with the sandbox platform this replaces, so a client
              written against that one drops in. Streaming exec has to support a closed input, because an
              agent process given an open one waits on it forever.
            </p>
          </div>
        </div>
      `,
    })}

    ${section({
      id: 'phases',
      heading: 'Which part of the drawing each phase added',
      lede: html`The build order is deliberate: correctness first, then speed, then a fleet, then the
        product face. Nothing above was designed as a first version to be replaced later, and each phase
        closes on a gate rather than on the code being written.`,
      body: html`
        <ol class="m-0 p-0 list-none flex flex-col">
          ${[
            ['1', 2, 'closed', 'Scaffold and contracts', 'The state schema, the API shape, the guest protocol, and the storage layout, all frozen before any parallel work started. Nothing on this page changed shape afterwards.'],
            ['2', 3, 'closed', 'Engine core', 'One box end to end: boot, exec, pause and resume, snapshot and restore, the router with wake-on-request, the idle monitor, and the isolation layer. Allowed to be slow, and it was.'],
            ['3', 4, 'closed', 'The instant engine', 'Everything under the storage and lazy-paging figures: content-addressed blocks, the header format, the two handlers, fault-order replay, and checkpoints that resume before they finish uploading.'],
            ['4', 5, 'closed', 'Cross-host and resilience', 'The gossiped replica replacing a local database, the encrypted mesh, any host serving any machine, and the self-heal loop with the standing-down rule.'],
            ['5', 15, 'closed', 'Volumes and the PaaS face', 'Durable volumes on object storage, the build pipeline, guest-to-guest naming, sealed environment values, and services with health-gated deploys. All three parts merged and the gate passed.'],
            ['6', 7, 'in progress', 'Product surface and sign-off', 'Tenancy, scoped keys and quotas on the API, typed clients, the hostility suite, and hugepage-backed guest memory have merged. The command line with the agent tool server, and the dashboard, are in review. Metering and the fleet sign-off remain.'],
          ].map(
            ([n, issue, status, title, body], i) => html`
              <li class="py-7 ${i > 0 ? 'border-t border-rule' : ''}">
                <div class="grid gap-4 wide:grid-cols-[3rem_1fr_auto] wide:gap-8 wide:items-start">
                  <span class="font-mono text-h2 leading-none text-ink-subtle">${n}</span>
                  <div>
                    <h3 class="text-h3 font-semibold m-0">${title}</h3>
                    <p class="text-sm text-ink-muted m-0 mt-2 max-w-[74ch]">${body}</p>
                  </div>
                  <div class="flex wide:flex-col items-start gap-2 wide:text-right">
                    <span
                      class="font-mono text-[10px] uppercase tracking-[0.14em] px-2 py-1 rounded-[2px] whitespace-nowrap
                             ${status === 'closed'
                               ? 'bg-signal text-signal-ink'
                               : status === 'in progress'
                                 ? 'border border-rule-strong text-ink'
                                 : 'border border-rule text-ink-subtle'}"
                      >${status}</span
                    >
                    <a
                      class="font-mono text-xs text-ink-subtle hover:text-ink no-underline whitespace-nowrap"
                      href="${GH_URL}/issues/${issue}"
                      target="_blank"
                      rel="noopener"
                      >issue #${issue}${NEW_TAB}</a
                    >
                  </div>
                </div>
              </li>
            `,
          )}
        </ol>
        <p class="${PROSE} mt-8">
          <a class=${LINK} href="/roadmap">The roadmap</a> carries each gate in full, including the ones
          that have not been met.
        </p>
      `,
    })}

    ${section({
      id: 'glossary',
      heading: 'The eight words this page leans on',
      lede: html`A technical page usually loses a reader on vocabulary rather than on ideas. These are
        the terms doing the work above, defined without pretending the simple version is the whole
        story.`,
      body: html`
        <dl class="m-0 grid gap-x-12 gap-y-0 wide:grid-cols-2">
          ${GLOSSARY.map(
            ([term, def], i) => html`
              <div class="py-5 ${i > 0 ? 'border-t border-rule' : ''} ${i === 1 ? 'wide:border-t-0' : ''}">
                <dt class="font-mono text-[13px] font-semibold m-0">${term}</dt>
                <dd class="text-sm text-ink-muted m-0 mt-2 max-w-[56ch] leading-[1.7]">${def}</dd>
              </div>
            `,
          )}
        </dl>
      `,
    })}

    ${section({
      id: 'numbers',
      layout: 'split',
      heading: 'Measured on a laptop, budgeted for metal',
      lede: html`Two sets of numbers, kept apart on purpose. The first were printed by the battery on a
        development rig whose disk cannot share extents, which makes them slower than the fleet should
        be. The second are targets that nothing has met yet, because there is no fleet.`,
      body: html`
        <div>
          <p class="${FIELD_LABEL} m-0">What the battery printed</p>
          <div class="grid gap-8 mt-5 mid:grid-cols-4">
            ${readout('createMeasured')} ${readout('wakeMeasured')} ${readout('resumeGapMeasured')}
            ${readout('assertions')}
          </div>
        </div>

        <hr class="${HAIRLINE} my-10" />

        <div>
          <p class="${FIELD_LABEL} m-0">What sign-off requires, and has not yet been run against</p>
          <div class="grid gap-8 mt-5 mid:grid-cols-3">
            ${readout('metalCreate')} ${readout('metalWake')} ${readout('metalPromote')}
          </div>
        </div>

        <p class="${PROSE} mt-10">
          The largest single change since the engine closed is guest memory backed by
          ${inlineFact('pageSize')} hugepages. On the same host, the same battery's checkpoint resume gap
          fell from ${inlineFact('resumeGapSmallPages')} to ${inlineFact('resumeGapMeasured')}, because
          the page size is recorded in every snapshot and a host that disagrees with the fleet refuses to
          restore rather than restoring slowly.
        </p>

        <p class="${PROSE} mt-6">
          The fleet numbers that matter are not in either group, because the fleet does not exist yet.
          The chaos gate is the closest thing there is: on a three-node rig, hard-killing the host that
          owned a machine returned it on a survivor in ${inlineFact('rescue')} with the same address and
          the disk intact, and a fourth host joined and started taking traffic ${inlineFact('join')}
          after one command. Both are correctness results rather than latency results, and they are the
          ones this design was actually built to produce.
        </p>

        <div class="mt-10 max-w-[54ch]">
          ${terminal('what a run reports', [
            { kind: 'cmd', text: 'PILOTS_E2E=1 npm test' },
            { kind: 'out', text: 'create, exec, checkpoint, restore, suspend, wake, destroy' },
            { kind: 'out', text: 'no orphaned processes, namespaces, slots or ports' },
            { kind: 'mark', text: 'the same battery, run against any host in the fleet' },
            { kind: 'note', text: 'later phases add assertions and never retire earlier ones' },
          ])}
        </div>

        <p class="${PROSE} mt-10">
          Everything drawn on this page is written down in full in the design document, and the code
          implementing it sits beside it. If a figure here disagrees with the repository, the repository
          is right and this page is a bug.
          <a class=${LINK} href=${GH_BOARD_URL} target="_blank" rel="noopener"
            >The board tracks what is left${NEW_TAB}</a
          >.
        </p>
      `,
    })}
  `;
}
