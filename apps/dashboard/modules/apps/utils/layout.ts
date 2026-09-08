/**
 * Where each service sits on an app's canvas, computed once and used twice.
 *
 * The canvas and the thumbnail on the app card are one picture at two zoom
 * levels, so they share one layout and one viewBox. That is only possible if
 * the layout is a pure function of the services and their edges, which is what
 * this module is: no DOM, no randomness, no state, the same input always
 * placing every card in the same spot.
 *
 * The rules, settled in #85 (D2):
 *
 *  - An edge runs from a service to a sibling it dials at <name>.internal. A
 *    name that is not a sibling in this app, or is the service itself, is not
 *    an edge; hostd already filters both, and this filters again so a stale
 *    or hand-built input cannot draw a line to nothing.
 *  - A cycle (web dials api, api dials web) is broken by dropping the edge
 *    whose SOURCE name sorts later, so the result is deterministic and the
 *    earlier-named service is the one that ends up above.
 *  - Longest-path layering: a service that dials nothing is layer 0; any other
 *    is one deeper than the deepest thing it dials. Layer 0 is the top row, so
 *    what a service dials sits above it and the arrow points up.
 *  - Within a layer, services are ordered by the mean x of what they dial
 *    (the barycenter), so a dependent lands under its dependencies rather than
 *    across the canvas from them, and ties are broken by name. One pass:
 *    layers are placed top to bottom, so the row above is already final.
 *  - A card is 240 by 120 units; columns are 288 apart and rows 216 apart,
 *    which leaves a gutter for the arrow and its head.
 */

export const CARD = { w: 240, h: 120, dx: 288, dy: 216 } as const;

export interface LayoutNode {
  id: string;
  name: string;
  /** Names of sibling services this one dials, as `Service.depends_on` says. */
  dependsOn: readonly string[];
}

export interface PlacedNode {
  id: string;
  name: string;
  x: number;
  y: number;
  layer: number;
}

/** From the dependent to what it dials, by service id. */
export interface LayoutEdge {
  from: string;
  to: string;
}

export interface AppLayout {
  /** In reading order: layer 0 first, then left to right within a layer. */
  placed: PlacedNode[];
  edges: LayoutEdge[];
  /** The viewBox the canvas and the thumbnail share. */
  width: number;
  height: number;
}

/** Plain code-point order, so the result does not depend on the server's locale. */
function byText(a: string, b: string): number {
  return a < b ? -1 : a > b ? 1 : 0;
}

type Edge = { from: string; to: string };

/**
 * Drop one edge per cycle until the graph is acyclic.
 *
 * A depth-first walk finds a back edge; the cycle is the stack from the target
 * of that edge to its source. Among the cycle's edges the one whose source
 * name sorts last goes, with the target name as the tie-break for a source
 * that appears twice. Repeating until the walk finds nothing terminates,
 * because every pass removes an edge and there are finitely many.
 */
function breakCycles(ids: string[], out: Map<string, Set<string>>, nameOf: (id: string) => string): void {
  const later = (a: Edge, b: Edge) => byText(nameOf(a.from), nameOf(b.from)) || byText(nameOf(a.to), nameOf(b.to));
  for (;;) {
    const cycle = findCycle(ids, out);
    if (!cycle) return;
    let drop = cycle[0]!;
    for (const edge of cycle) if (later(edge, drop) > 0) drop = edge;
    out.get(drop.from)!.delete(drop.to);
  }
}

function findCycle(ids: string[], out: Map<string, Set<string>>): Edge[] | null {
  const state = new Map<string, 'open' | 'done'>();
  const stack: string[] = [];
  const visit = (id: string): Edge[] | null => {
    state.set(id, 'open');
    stack.push(id);
    for (const target of out.get(id)!) {
      const seen = state.get(target);
      if (seen === 'open') {
        const ring = stack.slice(stack.indexOf(target));
        return ring.map((from, i) => ({ from, to: ring[(i + 1) % ring.length]! }));
      }
      if (seen === undefined) {
        const found = visit(target);
        if (found) return found;
      }
    }
    stack.pop();
    state.set(id, 'done');
    return null;
  };
  for (const id of ids) {
    if (!state.has(id)) {
      const found = visit(id);
      if (found) return found;
    }
  }
  return null;
}

export function layoutApp(nodes: readonly LayoutNode[]): AppLayout {
  const byId = new Map(nodes.map((n) => [n.id, n] as const));
  const nameOf = (id: string) => byId.get(id)!.name;
  // Ids in name order, so every walk below starts from the same place and a
  // tie anywhere resolves the same way on every render.
  const ids = [...byId.keys()].sort((a, b) => byText(nameOf(a), nameOf(b)) || byText(a, b));
  // Name to id. Names are unique within an org, so the first one wins and
  // that is only a tie-break in an input the engine would never produce.
  const idOf = new Map<string, string>();
  for (const id of ids) if (!idOf.has(nameOf(id))) idOf.set(nameOf(id), id);

  // Edges by id, in target-name order. Unknown targets and self-references
  // are not edges.
  const out = new Map<string, Set<string>>();
  for (const id of ids) {
    const targets = new Set<string>();
    for (const dep of [...new Set(byId.get(id)!.dependsOn)].sort(byText)) {
      const target = idOf.get(dep);
      if (target !== undefined && target !== id) targets.add(target);
    }
    out.set(id, targets);
  }
  breakCycles(ids, out, nameOf);

  // Longest path from a sink. Acyclic now, so the memoised walk terminates.
  const layerOf = new Map<string, number>();
  const layer = (id: string): number => {
    const known = layerOf.get(id);
    if (known !== undefined) return known;
    let depth = 0;
    for (const target of out.get(id)!) depth = Math.max(depth, layer(target) + 1);
    layerOf.set(id, depth);
    return depth;
  };
  for (const id of ids) layer(id);

  const rows = new Map<number, string[]>();
  for (const id of ids) {
    const l = layerOf.get(id)!;
    rows.set(l, [...(rows.get(l) ?? []), id]);
  }

  const x = new Map<string, number>();
  const placed: PlacedNode[] = [];
  for (const l of [...rows.keys()].sort((a, b) => a - b)) {
    const row = rows.get(l)!;
    // Every target is in a lower layer, so its x is already known.
    const mean = (id: string): number => {
      const targets = [...out.get(id)!];
      if (targets.length === 0) return 0;
      return targets.reduce((sum, t) => sum + x.get(t)!, 0) / targets.length;
    };
    row.sort((a, b) => mean(a) - mean(b) || byText(nameOf(a), nameOf(b)) || byText(a, b));
    row.forEach((id, index) => {
      const px = index * CARD.dx;
      x.set(id, px);
      placed.push({ id, name: nameOf(id), x: px, y: l * CARD.dy, layer: l });
    });
  }

  const edges: LayoutEdge[] = [];
  for (const id of ids) for (const target of out.get(id)!) edges.push({ from: id, to: target });

  const width = placed.reduce((w, p) => Math.max(w, p.x + CARD.w), 0);
  const height = placed.reduce((h, p) => Math.max(h, p.y + CARD.h), 0);
  return { placed, edges, width, height };
}
