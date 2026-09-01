import { html } from '@webjsdev/core';

/**
 * The o of pilots, as the mark.
 *
 * Same construction the sibling brand uses and the same reason for it: on
 * webjs.dev the standalone mark file and the lockup file carry byte-identical
 * path geometry and differ only in the viewBox, so the monogram is the logo
 * with the word cropped away rather than a second drawing to keep in step.
 * Every mark below is placed into the word at the o's own position, taken by
 * measuring the letter advances rather than by eye.
 *
 * WHY THE O IS THE PROMISING LETTER HERE. A circle is already the shape of
 * every instrument on a panel: the attitude indicator, the heading indicator,
 * the altimeter. So the one letter in the name that is a circle is also the
 * one that can carry the product's subject without illustrating anything. The
 * delta rounds never had that: an arrowhead had to be argued into being a
 * letter, and it never went quietly.
 *
 * THE WORD IS SET UPRIGHT HERE, unlike the delta lockups, and that is a
 * consequence rather than a preference. Those lean because the delta leans and
 * a mark cannot sit at one angle beside a word at another. Lean this word and
 * the o must lean with it, and a leaned circle is an ellipse, which is a
 * different letter drawn on purpose. The circle is worth more than the slant.
 *
 * The accent never touches these. Per the sibling brand's own note, the marks
 * carry no colour at all so the identity survives wherever the accent cannot
 * go, which is the whole point of rationing it.
 */

const ACCENT_INK = 'var(--logo-accent, currentColor)';

/** The delta, verbatim, for the variant that flies one inside the dial. */
const DELTA_D = 'M16 3.8 L26.8 27.6 L16 21.4 L5.2 27.6 Z';

/** Ring geometry on the 32-unit grid. Outer edge lands at 13.5, inner at 8.5. */
const R = 11;
const RING_W = 5;

export type OMark = {
  id: string;
  name: string;
  idea: string;
  cost: string;
  art: () => unknown;
};

const ring = () => html`
  <circle cx="16" cy="16" r=${R} fill="none" stroke=${ACCENT_INK} stroke-width=${RING_W} />
`;

export const O_MARKS: OMark[] = [
  {
    id: 'bezel',
    name: 'Bezel',
    idea:
      'The letter as a plain instrument bezel, cut across by the same slice the rest of the identity uses. It is the least the idea can be: a circle, and the house device applied to it.',
    cost:
      'A cut ring is two arcs facing each other, and that is a shape many things already are. Without the word beside it there is little here that says which product it belongs to.',
    art: () => html`
      ${ring()}
      <rect x="1" y="14.9" width="30" height="2.2" fill="var(--logo-bg)" />
    `,
  },
  {
    id: 'attitude',
    name: 'Attitude',
    idea:
      'The attitude indicator, the instrument that tells a pilot which way is up. Ground below the horizon with the fixed wing datum cut out of it, so the mark carries one horizon line and not three stacked strokes. The dial is genuinely a circle, so for once the mark is not being bent into a letter, it already was one.',
    cost:
      'It is the busiest of the four. The datum is the first thing to close up when the mark is small, and what survives is a circle half filled, which reads as a progress dial.',
    art: () => html`
      <!-- Ground: a chord of the inner disc, drawn explicitly rather than
           clipped so the mark carries no clip-path id. Ids collide the moment
           a page renders the same mark twice, and this one renders nine times.
           Half-chord is the square root of 8.5 squared minus 1 squared. -->
      <path d="M7.56 17 L24.44 17 A8.5 8.5 0 0 1 7.56 17 Z" fill=${ACCENT_INK} />
      <!-- The datum is CUT OUT of the ground rather than drawn above it. Set
           in the sky it becomes a third horizontal stroke under the horizon
           and the ring, and three stacked strokes in a circle read as a face.
           Punched into the ground there is only one horizon line left. -->
      <path
        d="M9.6 19.2 H12.9 L16 21.8 L19.1 19.2 H22.4"
        fill="none"
        stroke="var(--logo-bg)"
        stroke-width="2"
        stroke-linejoin="miter"
        stroke-linecap="butt"
      />
      ${ring()}
    `,
  },
  {
    id: 'heading',
    name: 'Heading',
    idea:
      'A heading indicator: the dial, an index notch cut into the rim at twelve, and the aircraft inside it. The aircraft is the delta from the candidate round at its own proportions, which lets the two marks that came out of this lab be the same drawing at two sizes.',
    cost:
      'A circle with something inside it is a badge, and a badge is the shape a design system outgrows. It also asks the delta to survive at a fraction of the size the delta was drawn for.',
    art: () => html`
      ${ring()}
      <!-- The index notch. Negative space rather than a tick added outside the
           rim, which would break the circle's silhouette and stop it reading
           as an o in the word. -->
      <rect x="14.6" y="1" width="2.8" height="5.4" fill="var(--logo-bg)" />
      <g transform="translate(16 16.6) scale(0.5) translate(-16.035 -15.7)">
        <g transform="translate(5.4 0)">
          <g transform="skewX(-11)"><path d=${DELTA_D} fill=${ACCENT_INK} /></g>
        </g>
      </g>
    `,
  },
  {
    id: 'marker',
    name: 'Marker',
    idea:
      'The dial with a single node set on its rim. One machine on a ring of identical positions, which is the architecture claim the earlier rounds kept failing to draw, said here without a diagram.',
    cost:
      'A ring with a bead on it is close to a clock face and closer still to a loading spinner. Where the node sits also has to be decided, and there is no reason in the product for any one position.',
    art: () => html`
      <!-- No slice here. Carrying it as well would make this Bezel with a bead
           added, and two candidates that differ by one element are one
           candidate. -->
      ${ring()}
      <rect x="19.4" y="3.4" width="7.4" height="7.4" rx="1.6" fill="var(--logo-bg)" />
      <rect x="20.6" y="4.6" width="5" height="5" rx="1.2" fill=${ACCENT_INK} />
    `,
  },
];

/** The mark on its own, square. */
export function oMark(v: OMark, px: number) {
  return html`
    <svg
      viewBox="0 0 32 32"
      width=${px}
      height=${px}
      class="block shrink-0"
      aria-hidden="true"
      focusable="false"
    >
      ${v.art()}
    </svg>
  `;
}

/**
 * Where every letter of the word sits, at font-size 139 on a baseline of 108.
 *
 * Measured with getExtentOfChar rather than guessed, which is the only way to
 * put a drawn glyph exactly where a typeset one would have gone. The lockup
 * sets "pil" and "ts" as two runs and drops the mark into the gap the o left,
 * so the spacing on both sides of it is the face's own and not a number
 * somebody nudged until it looked right.
 */
const FONT = 139;
const BASELINE = 108;
const ADV = { pil: 0, o: 147.5, ts: 227.6, end: 341.5 };
/** x-height for this face at this size, so the mark can be seated on it. */
const X_HEIGHT = 72.3;

/**
 * The lockup.
 *
 * `scale` is the mark's diameter as a multiple of the x-height. At 1 the mark
 * is exactly the size the typeset o would have been. Above 1 it overshoots,
 * which is what a logotype does when it wants the mark noticed, and what a
 * typeface does anyway by a percent or two so a round letter does not look
 * small beside a flat one.
 */
export function oLockup(v: OMark, opts: { height: number; scale?: number }) {
  const k = opts.scale ?? 1;
  const d = X_HEIGHT * k;
  const cx = (ADV.o + ADV.ts) / 2;
  const cy = BASELINE - X_HEIGHT / 2;
  const vb = { x: -14, y: -16, w: ADV.end + 28, h: 170 };
  const face =
    "font-family:ui-sans-serif,system-ui,-apple-system,'Segoe UI',Roboto,sans-serif;" +
    'font-weight:800;letter-spacing:-0.035em';
  return html`
    <svg
      viewBox="${vb.x} ${vb.y} ${vb.w} ${vb.h}"
      height=${opts.height}
      width=${Math.round((opts.height * vb.w) / vb.h)}
      role="img"
      aria-label="pilots"
      class="block"
    >
      <text x=${ADV.pil} y=${BASELINE} font-size=${FONT} fill="currentColor" style=${face}>pil</text>
      <text x=${ADV.ts} y=${BASELINE} font-size=${FONT} fill="currentColor" style=${face}>ts</text>
      <g
        transform="translate(${(cx - d / 2).toFixed(2)} ${(cy - d / 2).toFixed(2)}) scale(${(d / 32).toFixed(4)})"
      >
        ${v.art()}
      </g>
    </svg>
  `;
}
