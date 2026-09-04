/**
 * The sourced-number registry.
 *
 * AGENTS.md invariant 1: every digit-bearing claim on this site ships inside a
 * <data data-source="…"> element. This file is where the number and its
 * provenance live together, so the two cannot be separated by an edit to one
 * of them.
 *
 * The rule exists because rounded, unsourced numbers ("99.9% uptime", "10x
 * faster") are the loudest generated-copy tell there is: they come from a
 * process with no measurement behind it. A real number carrying a boring
 * source beats an impressive one carrying none.
 *
 * FOUR KINDS OF FACT, and conflating any two of them would be a lie:
 *
 *   kind: 'gate'      a threshold the build is required to meet, which a
 *                     CLOSED phase issue confirms it met. Rendered with a
 *                     "<" or similar and attributed to the gate.
 *   kind: 'design'    a fixed constant of the architecture (a block size, a
 *                     port, a table width). Not a measurement at all, and
 *                     never to be dressed up as performance.
 *   kind: 'measured'  a timing the battery actually printed, on hardware the
 *                     source string NAMES. Every one below was measured on a
 *                     development rig whose machine store cannot share
 *                     extents, which is slower than the fleet will be and is
 *                     stated rather than quietly omitted.
 *   kind: 'budget'    a target the production sign-off has to hit and has NOT
 *                     hit yet, because there is no production fleet. A budget
 *                     rendered as if it were a measurement is the exact lie
 *                     invariant 1 exists to prevent, so the kind is carried
 *                     separately and the source says which it is.
 *
 * The distinction between the last two is the one that matters most here. A
 * reader comparing this to a competitor's published latency is entitled to
 * know which of these numbers is a result and which is an intention.
 */
export type Fact = {
  /** Rendered text of the number itself, unit included. */
  value: string;
  /**
   * What it measures, in a few words.
   *
   * Keep it free of digits. `readout()` renders the label OUTSIDE the <data>
   * element, so a number here reaches the page without provenance and the
   * no-slop gate correctly fails it.
   */
  label: string;
  /**
   * Provenance. Goes into the data-source attribute verbatim.
   *
   * Write it without a full stop. Sentence-shaped string literals in a
   * non-template file are scanned as prose, and a source string is a citation
   * rather than a sentence.
   */
  source: string;
  kind: 'gate' | 'design' | 'measured' | 'budget';
};

export const FACTS = {
  create: {
    value: '<1.5s',
    label: 'create a machine from a template',
    source: 'Phase 3 gate, issue #4 (closed): timed on the laptop rig',
    kind: 'gate',
  },
  wake: {
    value: '<1s',
    label: 'wake a suspended machine',
    source: 'Phase 3 gate, issue #4 (closed): warm cache, timed on the laptop rig',
    kind: 'gate',
  },
  checkpoint: {
    value: '<500ms',
    label: 'checkpoint resume gap',
    source: 'Phase 3 gate, issue #4 (closed): timed on the laptop rig',
    kind: 'gate',
  },
  deadHost: {
    value: '30s',
    label: 'silence before a host is presumed dead',
    source: 'ARCHITECTURE.md, self-heal: hosts.last_seen threshold',
    kind: 'design',
  },
  idle: {
    value: '60s',
    label: 'default idle timer before suspend',
    source: 'ARCHITECTURE.md, idle monitor: per-machine default',
    kind: 'design',
  },
  slots: {
    value: '1024',
    label: 'network slots per host',
    source: 'ARCHITECTURE.md, constant-IP netns slot model',
    kind: 'design',
  },
  block: {
    value: '4KiB',
    label: 'content-addressed block size',
    source: 'ARCHITECTURE.md, header format: BlockSize=4096',
    kind: 'design',
  },
  processes: {
    value: '3',
    label: 'processes per host',
    source: 'ARCHITECTURE.md rule 2: hostd, corrosion, firecracker',
    kind: 'design',
  },

  /* Design constants the internals page names in prose. Each is a fixed part
     of a wire format or a protocol, not a measurement, and the source points
     at the paragraph of ARCHITECTURE.md that fixes it. */
  headerMeta: {
    value: '64',
    label: 'bytes of snapshot header metadata',
    source: 'ARCHITECTURE.md header format: Version, BlockSize, Size, Generation, BuildId, BaseBuildId',
    kind: 'design',
  },
  headerMap: {
    value: '40',
    label: 'bytes per block mapping',
    source: 'ARCHITECTURE.md header format: Offset, Length, BuildId, BuildStorageOffset',
    kind: 'design',
  },
  chainDepth: {
    value: '2',
    label: 'levels in a diff chain',
    source: 'ARCHITECTURE.md header format: template to per-machine diff, a grandparent reference is a hard error',
    kind: 'design',
  },
  agentPort: {
    value: '3001',
    label: 'guest agent port',
    source: 'ARCHITECTURE.md guest-agent protocol',
    kind: 'design',
  },
  faultWorkers: {
    value: '4',
    label: 'page-fault workers per machine',
    source: 'ARCHITECTURE.md lazy memory: uffd handler worker count',
    kind: 'design',
  },
  gossipMtu: {
    value: '1232',
    label: 'bytes of pinned gossip datagram',
    source: 'Phase 4 issue #5 (closed): minimum WireGuard MTU less the IPv6 and UDP headers',
    kind: 'design',
  },
  pageSize: {
    value: '2MiB',
    label: 'guest page size, fleet-wide',
    source: 'ARCHITECTURE.md rule 8: PILOT_HUGEPAGES, recorded in every snapshot and never reinterpreted at restore',
    kind: 'design',
  },
  settle: {
    value: '20s',
    label: 'guest settle before a template is captured',
    source: 'ARCHITECTURE.md process management: snapshot only after the guest reaches system-running',
    kind: 'design',
  },

  /* Measured. The rig is named in every source string because it is the whole
     caveat: no reflink support, so every copy is a real copy. */
  createMeasured: {
    value: '142ms',
    label: 'create, median',
    source: 'Phase 6 perf PR #28, merged 2026-09-04: scripts/e2e.mjs on a nested-KVM host with 2MiB hugepages, no reflink support',
    kind: 'measured',
  },
  wakeMeasured: {
    value: '255ms',
    label: 'wake, median',
    source: 'Phase 6 perf PR #28, merged 2026-09-04: scripts/e2e.mjs on a nested-KVM host with 2MiB hugepages, no reflink support',
    kind: 'measured',
  },
  resumeGapMeasured: {
    value: '300ms',
    label: 'checkpoint resume gap, median',
    source: 'Phase 6 perf PR #28, merged 2026-09-04: scripts/e2e.mjs on a nested-KVM host with 2MiB hugepages, no reflink support',
    kind: 'measured',
  },
  resumeGapSmallPages: {
    value: '3726ms',
    label: 'the same resume gap without hugepages',
    source: 'Phase 6 perf PR #28, merged 2026-09-04: the same battery on the same host at 4KiB pages',
    kind: 'measured',
  },
  assertions: {
    value: '104',
    label: 'steps in the battery',
    source: 'scripts/e2e.mjs on main, counted 2026-09-04: await step calls, beside 22 numbered sections in scripts/cluster/gate.sh',
    kind: 'measured',
  },
  rescue: {
    value: '125s',
    label: 'to rescue a hard-killed host’s machines',
    source: 'Phase 4 issue #5 (closed): scripts/cluster/gate.sh step 7 on the three-node nested-KVM rig',
    kind: 'measured',
  },
  join: {
    value: '15s',
    label: 'for a new host to be live and counted',
    source: 'Phase 4 issue #5 (closed): scripts/cluster/gate.sh step 8, one host-bootstrap.sh run',
    kind: 'measured',
  },
  rootfsCopy: {
    value: '2196ms',
    label: 'to duplicate the golden rootfs without reflink',
    source: 'ARCHITECTURE.md engine mechanics: the ext4 finding behind the Phase 4 create overrun',
    kind: 'measured',
  },
  ext4Create: {
    value: '2.6s',
    label: 'create on a store that cannot share extents',
    source: 'Phase 4 issue #5 (closed): battery run on the nested-KVM rig, flat across every node',
    kind: 'measured',
  },
  prefaultCold: {
    value: '5.8s',
    label: 'first checkpoint without a resident-memory pass',
    source: 'ARCHITECTURE.md snapshot step 2: guest memory faulted through the handler with the guest frozen',
    kind: 'measured',
  },
  prefaultWarm: {
    value: '450ms',
    label: 'the same checkpoint with one',
    source: 'ARCHITECTURE.md snapshot step 2: memory made resident before the pause',
    kind: 'measured',
  },

  /* Budgets. Not results. The source says so in words, because a reader
     skimming a table of numbers will not infer it from a field name. */
  metalCreate: {
    value: '<500ms',
    label: 'create',
    source: 'Phase 6 issue #7 sign-off budget, NOT yet measured: p50 on the Hetzner fleet',
    kind: 'budget',
  },
  metalWake: {
    value: '<200ms',
    label: 'wake',
    source: 'Phase 6 issue #7 sign-off budget, NOT yet measured: p50 on the Hetzner fleet',
    kind: 'budget',
  },
  metalRelease: {
    value: '<1s',
    label: 'start a replica, roll back, or scale up',
    source: 'Phase 6 issue #7 sign-off budget, NOT yet measured: p50 restore from a release on the Hetzner fleet',
    kind: 'budget',
  },
  metalPromote: {
    value: '<1.5s',
    label: 'promote a sandbox to a service',
    source: 'Phase 6 issue #7 sign-off budget, NOT yet measured: p50 on the Hetzner fleet',
    kind: 'budget',
  },
} as const satisfies Record<string, Fact>;

export type FactKey = keyof typeof FACTS;
