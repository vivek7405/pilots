import { html } from '@webjsdev/core';

/**
 * The drawing kit for the internals page.
 *
 * Why hand-authored inline SVG rather than a rendering library: a diagram on
 * this site has to survive the same constraints the rest of it does. It must
 * read in both themes from ONE declaration (so every stroke and fill is a
 * palette token, never a literal hex), it must cost no request and no script,
 * and it must stay legible when a reader zooms. A library would give up all
 * three to save markup that is written once.
 *
 * The parts here are deliberately primitive: a box, a rule, an arrow, a label.
 * Each diagram is then LAID OUT by hand on an explicit coordinate grid, because
 * an auto-layout engine draws the graph it was given rather than the mechanism
 * the reader needs to see, and the difference is the whole value of the figure.
 *
 * Two constraints that are easy to violate and produce a broken page:
 *
 * - **Text inside an `<svg>` is text a reader sees**, so the no-slop gate scans
 *   it exactly like prose in a paragraph. A unit-bearing number in a label
 *   ("30s") fails invariant 1 the same way it would in a sentence. Diagram
 *   labels therefore name the thing rather than quoting a measurement, and the
 *   measurement goes in the caption through the sourced-facts registry.
 * - **Marker ids are document-global.** Two figures on one page each defining
 *   `#arrow` would silently share whichever the parser met first. Rather than
 *   prefixing an id per figure and threading it through forty arrow calls,
 *   `arrowDefs()` is rendered ONCE per page and every arrow references it.
 *   Document-global is the property that makes that work instead of the bug it
 *   would otherwise be.
 *
 * Accent use follows AGENTS.md invariant 2. Acid green marks ONE thing in a
 * figure, the path or the boundary the figure exists to show, and it is a
 * stroke or a hairline rather than a fill behind type: it fails contrast under
 * dark text on paper.
 */

/** Monospace label, the instrument-panel voice every figure speaks in. */
const LABEL = 'font-mono text-[12px] fill-ink';
const LABEL_SM = 'font-mono text-[10.5px] fill-ink-muted';
const LABEL_XS = 'font-mono text-[9.5px] fill-ink-subtle';

export type Tone =
  /** The ordinary case: a component drawn on the page. */
  | 'plain'
  /** A component the figure's claim is about. Heavier rule, no accent. */
  | 'strong'
  /** The one element the figure exists to point at. Accent hairline. */
  | 'signal'
  /** Context that is present but not the subject: storage, a remote host. */
  | 'sunken'
  /** Something that is gone, dead, or refused. */
  | 'dead';

const TONE_RECT: Record<Tone, string> = {
  plain: 'fill-paper-elev stroke-rule',
  strong: 'fill-paper-elev stroke-rule-strong',
  signal: 'fill-paper-elev stroke-signal',
  sunken: 'fill-paper-sunken stroke-rule',
  dead: 'fill-paper-sunken stroke-rule [stroke-dasharray:3_3]',
};

/**
 * A labelled box.
 *
 * `sub` is a second line inside the box, for the detail that makes the label
 * mean something (a port, a path, a wire format). Both lines are centred on the
 * box, so a caller only positions the box.
 */
export function box(o: {
  x: number;
  y: number;
  w: number;
  h: number;
  label: string;
  sub?: string;
  tone?: Tone;
  /** Rendered smaller, for a box that is a member of a stack rather than a component. */
  small?: boolean;
}) {
  const tone = o.tone ?? 'plain';
  const cx = o.x + o.w / 2;
  const cy = o.y + o.h / 2;
  const labelClass = o.small ? LABEL_SM : LABEL;
  return html`
    <g>
      <rect
        x=${o.x}
        y=${o.y}
        width=${o.w}
        height=${o.h}
        rx="3"
        class="${TONE_RECT[tone]}"
        stroke-width="1"
      ></rect>
      ${o.sub
        ? html`
            <text x=${cx} y=${cy - 3} text-anchor="middle" class=${labelClass}>${o.label}</text>
            <text x=${cx} y=${cy + 11} text-anchor="middle" class=${LABEL_XS}>${o.sub}</text>
          `
        : html`<text x=${cx} y=${cy + 4} text-anchor="middle" class=${labelClass}>${o.label}</text>`}
    </g>
  `;
}

/**
 * A grouping frame: an outlined region enclosing several boxes, captioned at
 * its top-left corner rather than centred, so the caption cannot be mistaken
 * for a component of its own.
 */
export function frame(o: {
  x: number;
  y: number;
  w: number;
  h: number;
  label: string;
  tone?: Tone;
  children?: unknown;
}) {
  return html`
    <g>
      <rect
        x=${o.x}
        y=${o.y}
        width=${o.w}
        height=${o.h}
        rx="4"
        class="${TONE_RECT[o.tone ?? 'sunken']}"
        stroke-width="1"
      ></rect>
      <text x=${o.x + 10} y=${o.y + 16} class=${LABEL_SM}>${o.label}</text>
      ${o.children ?? ''}
    </g>
  `;
}

/**
 * The page's arrowheads, rendered ONCE near the top of the document.
 *
 * A marker is referenced by a document-global id, so defining these per figure
 * would mean every figure after the first silently drew the first figure's
 * markers. Defining them once and referencing them everywhere turns that same
 * property into the mechanism.
 *
 * The carrier `<svg>` is sized to nothing rather than hidden with `display:
 * none`, which is the version that keeps referenced markers resolvable.
 */
export function arrowDefs() {
  return html`
    <svg width="0" height="0" aria-hidden="true" class="absolute overflow-hidden" focusable="false">
      <defs>
        <marker
          id="dg-head"
          viewBox="0 0 8 8"
          refX="7"
          refY="4"
          markerWidth="7"
          markerHeight="7"
          orient="auto-start-reverse"
        >
          <path d="M 0 0 L 8 4 L 0 8 z" class="fill-ink-muted"></path>
        </marker>
        <marker
          id="dg-head-signal"
          viewBox="0 0 8 8"
          refX="7"
          refY="4"
          markerWidth="7"
          markerHeight="7"
          orient="auto-start-reverse"
        >
          <path d="M 0 0 L 8 4 L 0 8 z" class="fill-signal"></path>
        </marker>
      </defs>
    </svg>
  `;
}

export type ArrowKind =
  /** An ordinary call or data movement. */
  | 'solid'
  /** Something continuous and background: gossip, a heartbeat, replication. */
  | 'dashed'
  /** The path the figure is about. */
  | 'signal';

const ARROW_CLASS: Record<ArrowKind, string> = {
  solid: 'stroke-ink-muted',
  dashed: 'stroke-ink-subtle [stroke-dasharray:4_4]',
  signal: 'stroke-signal',
};

/**
 * A labelled arrow.
 *
 * `d` is a path, not a pair of points, because the interesting arrows in these
 * figures are not straight: a request that leaves a host and comes back needs
 * to be drawn going around the thing it did not go through.
 *
 * An unlabelled arrow says "related somehow", so `label` is required. Where a
 * label genuinely has no room, the caption carries it instead and the arrow
 * gets the short form.
 */
export function arrow(o: {
  d: string;
  label: string;
  /** Where the label sits. Defaults above the midpoint of a horizontal run. */
  lx: number;
  ly: number;
  kind?: ArrowKind;
  anchor?: 'start' | 'middle' | 'end';
  /** Draw a head at the start too, for a two-way path. */
  both?: boolean;
}) {
  const kind = o.kind ?? 'solid';
  const head = kind === 'signal' ? 'url(#dg-head-signal)' : 'url(#dg-head)';
  return html`
    <g>
      <path
        d=${o.d}
        fill="none"
        stroke-width="1.25"
        class="${ARROW_CLASS[kind]}"
        marker-end=${head}
        marker-start=${o.both ? head : 'none'}
      ></path>
      <text x=${o.lx} y=${o.ly} text-anchor=${o.anchor ?? 'middle'} class=${LABEL_XS}>${o.label}</text>
    </g>
  `;
}

/** Free-standing text, for a note that is not attached to a box. */
export function note(o: {
  x: number;
  y: number;
  text: string;
  anchor?: 'start' | 'middle' | 'end';
  strong?: boolean;
}) {
  return html`<text
    x=${o.x}
    y=${o.y}
    text-anchor=${o.anchor ?? 'start'}
    class=${o.strong ? LABEL_SM : LABEL_XS}
  >${o.text}</text>`;
}

/**
 * The figure shell.
 *
 * Every diagram is wrapped in a `<figure>` with a caption that states the ONE
 * claim the picture makes, and the `<svg>` carries that same claim as its
 * aria-label. A reader who cannot see the drawing gets the sentence rather than
 * a list of box names, which is what the drawing is for in the first place.
 *
 * The horizontal scroll container is not optional. These figures are wide, and
 * a wide drawing squeezed into a phone viewport becomes unreadable type rather
 * than a smaller picture. Scrolling the figure keeps the page body from
 * scrolling sideways, which is the failure this replaces.
 */
export function figure(o: {
  /** The claim, in a sentence. Becomes both the aria-label and the caption. */
  label: string;
  /** Optional longer caption. When absent the label carries the figure alone. */
  caption?: unknown;
  viewBox: string;
  /**
   * A Tailwind min-width. Below it the figure SCROLLS rather than shrinking:
   * a wide drawing squeezed into a phone viewport stops being a smaller
   * picture and becomes unreadable type.
   */
  minW: string;
  body: unknown;
}) {
  return html`
    <figure class="m-0">
      <div class="rounded border border-rule bg-paper-elev px-4 py-5 mid:px-6 overflow-x-auto scroll-thin">
        <svg
          role="img"
          aria-label=${o.label}
          viewBox=${o.viewBox}
          class="w-full h-auto ${o.minW}"
          xmlns="http://www.w3.org/2000/svg"
        >
          ${o.body}
        </svg>
      </div>
      <figcaption class="mt-3 text-sm text-ink-muted max-w-[74ch]">${o.caption ?? o.label}</figcaption>
    </figure>
  `;
}
