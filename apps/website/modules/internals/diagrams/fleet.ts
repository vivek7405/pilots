import { html } from '@webjsdev/core';
import { box, frame, arrow, note, figure, type Tone } from '#lib/ui/diagram.ts';
import { inlineFact } from '#lib/ui/stat.ts';

/**
 * Figure: the fleet.
 *
 * The claim it exists to make is a NEGATIVE one, which is the hard kind to
 * draw: there is no scheduler tier, no managed database, and no load balancer
 * appliance. A picture cannot show an absence, so this one shows the three
 * things that would have to be there instead, all of them replicated per host:
 * the API surface, the state replica, and the storage client. A reader who
 * looks for the box in the middle and does not find one has read the figure.
 */
export function fleetFigure() {
  /**
   * Positions and paths are computed HERE rather than interpolated inside the
   * html`` template below, and that is not a style preference. The no-slop
   * gate's markup scanner matches a template lazily to the first backtick it
   * meets, so a nested backtick literal inside one truncates the match and
   * leaves the following TypeScript being read as prose. It reported a
   * semicolon in a type annotation. Building the strings first keeps every
   * backtick outside the markup, where it belongs anyway.
   */
  const hosts = [20, 325, 630].map((x, i) => ({
    x,
    label: 'host ' + ['fsn1-a', 'fsn1-b', 'nbg1-a'][i],
    fromDns: 'M ' + (x + 125) + ' 52 V 96',
    toStore: 'M ' + (x + 125) + ' 302 V 328',
  }));

  return figure({
    label:
      'Three identical hosts, each running the full stack, gossiping state to each other and reading it locally. There is no coordinating tier between them.',
    viewBox: '0 0 900 400',
    minW: 'min-w-[720px]',
    body: html`
      ${box({
        x: 330,
        y: 8,
        w: 240,
        h: 40,
        label: 'wildcard DNS',
        sub: 'A records for every host',
        tone: 'sunken',
      })}

      ${hosts.map((h) => arrow({ d: h.fromDns, label: '', lx: 0, ly: 0, kind: 'signal' }))}
      ${note({ x: 180, y: 78, text: 'any host answers the whole API', anchor: 'start' })}

      ${hosts.map((h) => frame({ x: h.x, y: 100, w: 250, h: 160, label: h.label }))}
      ${hosts.map(
        (h) => html`
          ${box({ x: h.x + 20, y: 122, w: 210, h: 32, label: 'hostd', tone: 'strong', small: true })}
          ${box({ x: h.x + 20, y: 162, w: 210, h: 32, label: 'corrosion', small: true })}
          ${box({ x: h.x + 20, y: 202, w: 210, h: 32, label: 'firecracker, one per machine', small: true })}
        `,
      )}

      ${arrow({ d: 'M 272 180 H 321', label: 'gossip', lx: 296, ly: 172, kind: 'dashed', both: true })}
      ${arrow({ d: 'M 577 180 H 626', label: 'gossip', lx: 601, ly: 172, kind: 'dashed', both: true })}
      ${arrow({
        d: 'M 145 264 C 145 300, 755 300, 755 264',
        label: 'every host holds every row',
        lx: 290,
        ly: 318,
        kind: 'dashed',
        both: true,
      })}

      ${hosts.map((h) => arrow({ d: h.toStore, label: '', lx: 0, ly: 0 }))}
      ${box({
        x: 250,
        y: 332,
        w: 400,
        h: 46,
        label: 'object storage',
        sub: 'chunks, volumes, certificates',
        tone: 'sunken',
      })}
    `,
    caption: html`Every host runs the same ${inlineFact('processes')} processes, holds a full replica of
      the fleet's state, and can answer an API call for a machine it has never run. The dashed edges
      are gossip rather than requests, so a partitioned host keeps serving out of its local replica.
      Object storage holds the only authoritative copy of machine state, which is what makes the
      arrows between hosts optional rather than load-bearing.`,
  });
}

/**
 * Figure: the split brain, and how it resolves without an arbiter.
 *
 * This is the sharpest edge in the whole design and the one a reader coming
 * from a consensus system will look for immediately. The state layer merges
 * last-write-wins COLUMN BY COLUMN, so a returning owner and its rescuer can
 * momentarily disagree about a row without anything erroring. The resolution
 * is not an election. It is a rule every host runs against its own machines on
 * every tick, and the fourth panel is the whole point: the host that loses the
 * row is the host that shuts down, so the disagreement resolves toward the
 * state the row already describes.
 */
export function splitBrainFigure() {
  /**
   * The x offset is baked into the array rather than derived inside the map,
   * for the reason fleetFigure() records: a statement inside a callback that
   * also opens a nested html`` template lands in the gate's truncated markup
   * match and is read as prose. Expression bodies only, below.
   */
  const panels: {
    x: number;
    label: string;
    a: Tone;
    b: Tone;
    row: string;
    sub: string;
  }[] = [
    { x: 20, label: 'healthy', a: 'plain', b: 'plain', row: 'the machine row', sub: 'host_id names A' },
    { x: 245, label: 'A is partitioned', a: 'dead', b: 'plain', row: 'the machine row', sub: 'unchanged, and stale' },
    { x: 470, label: 'B rescues the slice', a: 'dead', b: 'strong', row: 'one transaction', sub: 'host_id and state together' },
    { x: 695, label: 'A returns, and stands down', a: 'signal', b: 'strong', row: 'the machine row', sub: 'host_id names B' },
  ];

  return figure({
    label:
      'A partitioned host is rescued by a survivor, and when the original host returns it reads the row, sees another host named as owner, and kills its own copy of the machine.',
    viewBox: '0 0 900 232',
    minW: 'min-w-[760px]',
    body: html`
      ${panels.map((p) => frame({ x: p.x, y: 30, w: 205, h: 160, label: p.label }))}
      ${panels.map(
        (p) => html`
          ${box({ x: p.x + 12, y: 62, w: 84, h: 34, label: 'host A', tone: p.a, small: true })}
          ${box({ x: p.x + 109, y: 62, w: 84, h: 34, label: 'host B', tone: p.b, small: true })}
          ${box({ x: p.x + 12, y: 120, w: 181, h: 46, label: p.row, sub: p.sub, small: true })}
        `,
      )}
      ${note({ x: 898, y: 210, text: 'A kills its own copy and frees the slot', anchor: 'end' })}
    `,
    caption: html`Merges happen per column, so a returning owner can heartbeat a row another host has
      already claimed. Two rules make that safe. The rescuer writes owner and state in a single
      transaction, and on every tick a host kills any machine it is locally running whose row now
      names somebody else. Nothing votes, and nothing waits for a quorum, so a host that is wrong is
      corrected by reading rather than by being told.`,
  });
}
