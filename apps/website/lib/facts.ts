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
 * TWO KINDS OF FACT, and conflating them would be a lie:
 *
 *   kind: 'gate'      a threshold the build is required to meet, which a
 *                     CLOSED phase issue confirms it met. Rendered with a
 *                     "<" or similar and attributed to the gate.
 *   kind: 'design'    a fixed constant of the architecture (a block size, a
 *                     port, a table width). Not a measurement at all, and
 *                     never to be dressed up as performance.
 *
 * There is deliberately no kind: 'benchmark' yet. When the Hetzner fleet is
 * up and scripts/e2e.mjs prints real timings, they land here as one, and the
 * gate numbers below become the floor rather than the headline.
 */
export type Fact = {
  /** Rendered text of the number itself, unit included. */
  value: string;
  /** What it measures, in a few words. */
  label: string;
  /** Provenance. Goes into the data-source attribute verbatim. */
  source: string;
  kind: 'gate' | 'design';
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
} as const satisfies Record<string, Fact>;

export type FactKey = keyof typeof FACTS;
