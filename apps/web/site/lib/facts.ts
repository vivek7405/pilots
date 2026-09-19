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
 * THREE KINDS OF FACT, and conflating any two of them would be a lie:
 *
 *   kind: 'design'    a fixed constant of the architecture (a block size, a
 *                     port, a table width). Not a measurement at all, and
 *                     never to be dressed up as performance.
 *   kind: 'measured'  a timing the battery actually printed, on hardware the
 *                     source string NAMES. Most are from the production fleet;
 *                     the ones that need a host killed or a fleet setting
 *                     changed are from a test cluster, and the two are not
 *                     comparable. The source says which.
 *   kind: 'budget'    a target the fleet is held to. Whether the fleet is
 *                     inside it is in the source string. A budget
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
  kind: 'design' | 'measured' | 'budget';
};

export const FACTS = {
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
    source: 'the Corrosion config host-bootstrap.sh writes, max_mtu: minimum WireGuard MTU less the IPv6 and UDP headers',
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

  /* Measured. The hardware is named in every source string because it is the
     whole caveat. */
  createMeasured: {
    value: '468ms',
    label: 'create, median on metal',
    source: 'production fleet, 2026-09-18: the timing section of scripts/e2e.mjs under PILOTS_E2E_METAL=1, run on a Hetzner i7-6700 host, median of five',
    kind: 'measured',
  },
  wakeMeasured: {
    value: '302ms',
    label: 'wake, median on metal',
    source: 'production fleet, 2026-09-18: the timing section of scripts/e2e.mjs under PILOTS_E2E_METAL=1, run on a Hetzner i7-6700 host, median of five',
    kind: 'measured',
  },
  resumeGapMeasured: {
    value: '403ms',
    label: 'checkpoint resume gap, median on metal',
    source: 'production fleet, 2026-09-18: the timing section of scripts/e2e.mjs under PILOTS_E2E_METAL=1, run on a Hetzner i7-6700 host, median of five',
    kind: 'measured',
  },
  resumeGapSmallPages: {
    value: '3726ms',
    label: 'the same resume gap without hugepages',
    source: 'scripts/e2e.mjs on a nested-KVM test host, 2026-09-04: the same battery on the same host at 4KiB pages',
    kind: 'measured',
  },
  rescue: {
    value: '125s',
    label: 'to rescue a hard-killed host’s machines',
    source: 'scripts/cluster/gate.sh, the dead-host section, on a three-host test cluster: the battery kills a host on purpose',
    kind: 'measured',
  },
  join: {
    value: '15s',
    label: 'for a new host to be live and counted',
    source: 'scripts/cluster/gate.sh, the join section, on a three-host test cluster: one host-bootstrap.sh run',
    kind: 'measured',
  },
  resumeGapHugepages: {
    value: '300ms',
    label: 'the same resume gap with hugepages, on a test host',
    source: 'scripts/e2e.mjs on a nested-KVM test host, 2026-09-04: 2MiB hugepages',
    kind: 'measured',
  },
  urlWake: {
    value: '352ms',
    label: 'wake of a real site through its public address, median, network removed',
    source: 'production fleet, 2026-09-18: the WebJs website at half a gibibyte, one request after the platform reported it suspended, cold server wait minus warm server wait, five rounds',
    kind: 'measured',
  },
  rootFlushPauseMetal: {
    value: '250ms',
    label: 'ceiling on every root flush pause one metal host recorded',
    source: 'production fleet, 2026-09-18: pilots_root_flush_pause_seconds on a Hetzner i7-6700 host over eighteen flushes, of which four were inside the budget',
    kind: 'measured',
  },
  rootFlushWindow: {
    value: '60s',
    label: 'at most between a write to a machine root and the bucket holding it',
    source: 'ARCHITECTURE.md two durability tiers: the PILOT_ROOT_FLUSH_INTERVAL default, and the e2e battery asserts the realised lag against it',
    kind: 'design',
  },
  rootFlushPause: {
    value: '25ms',
    label: 'guest pause a root flush is allowed to cost, high percentile across a fleet',
    source: 'ARCHITECTURE.md two durability tiers: the SLO on pilots_root_flush_pause_seconds, a fleet target the battery bounds per flush',
    kind: 'budget',
  },
  bucketStream: {
    value: '55 MB/s',
    label: 'one upload stream from a host to object storage in the same datacentre',
    source: 'production fleet, 2026-09-18: one PUT of half a gibibyte from a Hetzner host to the Falkenstein bucket',
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
  /* How long a request to a SLEEPING machine is held before it is given up on.
     A design constant rather than a measurement: it is the ceiling, and the
     thing worth publishing about it is that there is only one of it. Wherever
     the machine is, the wait is bounded the same way, so nobody can tell which
     host holds their sandbox from how long a cold request takes. */
  heldWake: {
    value: '120s',
    label: 'how long a request to a sleeping machine is held',
    source: 'router.HeldWakeWindow, one constant for the same-host wake and the cross-host forward; apps/hostd/internal/router/heldwake_test.go asserts they cannot diverge',
    kind: 'design',
  },
  metalCreate: {
    value: '<500ms',
    label: 'create',
    source: 'fleet target, as a median on the Hetzner fleet: met on 2026-09-18',
    kind: 'budget',
  },
  metalWake: {
    value: '<200ms',
    label: 'wake',
    source: 'fleet target, as a median on the Hetzner fleet: the 2026-09-18 median is above it',
    kind: 'budget',
  },
  metalRelease: {
    value: '<1s',
    label: 'start a replica, roll back, or scale up',
    source: 'fleet target: median restore from a release on the Hetzner fleet',
    kind: 'budget',
  },
} as const satisfies Record<string, Fact>;

export type FactKey = keyof typeof FACTS;
