import { html } from '@webjsdev/core';

/**
 * The delta raked into the P of pilots, with its flap overhanging the i.
 *
 * ONE DRAWING, TWO CROPS, which is the sibling brand's construction: on
 * webjs.dev the standalone mark file and the lockup file carry byte-identical
 * path geometry and differ only in the viewBox, so the monogram is the logo
 * with the word cropped off rather than a second drawing to keep in sync.
 *
 * WHY THE APEX IS RAKED, because it is the whole reason this round exists and
 * it is a geometric fact rather than a preference.
 *
 * Rotating the delta exactly ninety degrees puts its apex at the bowl's
 * mid-height. Every point on the trailing edge left of that apex is therefore
 * LOWER than the apex, so the flap descends as it approaches the stem. An
 * x-height letter starts around a third of the way down the cap, which means
 * a bowl of any usable size collides with it long before it could hang over
 * it. Shortening the bowl until it clears leaves a bowl too small to read as
 * a P. There is no setting of the ninety-degree version that overhangs.
 *
 * So the delta is SHEARED: the wingtips still sit on the stem, but the apex is
 * lifted above the midpoint by `rake`. Now the trailing edge falls away to the
 * right of the letter rather than towards it, the flap tip is the highest
 * thing on the bowl, and the space underneath is free for the next letter to
 * tuck into.
 *
 * THE i IS DOTLESS, and that is the trade this construction makes. The zone a
 * tittle occupies is exactly the zone the flap sweeps through. Rather than
 * fight for it, the flap takes the job: it is the mark and the i's tittle at
 * once, which is what closes the gap the previous round left open.
 *
 * The coordinate space, stated once:
 *
 *   cap top   y = 8        baseline  y = 108      (cap height 100)
 *   stem      x = 18 to 44                        (26 wide, 0.26 of cap)
 *   bowl      y = 8 to bowlBottom, apex raked above the midpoint
 */

const ACCENT_INK = 'var(--logo-accent, currentColor)';

const CAP_TOP = 8;
const BASELINE = 108;
const STEM_L = 18;
const STEM_R = 44;
const SLICE_H = 9;

/** tan(9 degrees). The lean, applied to the letter and the word alike. */
const TAN9 = 0.15838;
const LEAN = 'skewX(-9)';

/**
 * The delta's own aspect, length over wingtip span, taken off the candidate
 * drawing. The mark is very nearly square, and deriving the bowl's length from
 * its height through this number rather than fixing an apex coordinate is what
 * stops the rotated delta stretching into a pennant.
 */
const DELTA_ASPECT = 23.8 / 21.6;

export type DeltaP = {
  id: string;
  name: string;
  idea: string;
  cost: string;
  /** Where the bowl closes back onto the stem. */
  bowlBottom: number;
  /** How far the apex is lifted above the wingtips' midpoint. Zero is a plain rotation. */
  rake: number;
  /** Multiplier on the delta's natural length. Above one is overhang bought with width. */
  reach: number;
  /** How far the tail notch cuts back along the axis, as a fraction of it. */
  notch: number;
  /** Vertical thickness where each blade lands on the stem. Zero keeps the delta's points. */
  blunt: number;
};

function apexOf(v: DeltaP) {
  const x = STEM_R + (v.bowlBottom - CAP_TOP) * DELTA_ASPECT * v.reach;
  const y = (CAP_TOP + v.bowlBottom) / 2 - v.rake;
  return { x, y };
}

/**
 * The bowl.
 *
 * Traced from the top join out to the apex, back to the bottom join, then in
 * and out of the notch. The notch apex sits on the delta's own axis, the line
 * from the wingtips' midpoint to the apex, so shearing the shape carries the
 * notch with it instead of leaving it pointing somewhere the mark no longer is.
 */
function bowlPath(v: DeltaP): string {
  const a = apexOf(v);
  const midY = (CAP_TOP + v.bowlBottom) / 2;
  const nx = STEM_R + (a.x - STEM_R) * v.notch;
  const ny = midY + (a.y - midY) * v.notch;
  const b = v.blunt;
  return [
    `M${STEM_R} ${CAP_TOP}`,
    `L${a.x.toFixed(2)} ${a.y.toFixed(2)}`,
    `L${STEM_R} ${v.bowlBottom}`,
    `L${STEM_R} ${v.bowlBottom - b}`,
    `L${nx.toFixed(2)} ${ny.toFixed(2)}`,
    `L${STEM_R} ${CAP_TOP + b}`,
    'Z',
  ].join(' ');
}

/**
 * The letter, as separate paths: the bowl, and the stem in one piece or two.
 *
 * When sliced, the cut starts exactly where the bowl closes, so the split runs
 * through the stem alone and no concave polygon has to be divided. The gap is
 * absent geometry rather than a bar painted in the background colour, which is
 * what lets one file sit on ink, on paper, and on a photograph.
 */
export function deltaPLetter(v: DeltaP, opts: { sliced?: boolean } = {}) {
  const sliced = opts.sliced ?? true;
  const y = v.bowlBottom;
  return html`
    <g transform=${LEAN}>
      <path d=${bowlPath(v)} fill=${ACCENT_INK} />
      <path
        d="M${STEM_L} ${CAP_TOP} H${STEM_R} V${sliced ? y : BASELINE} H${STEM_L} Z"
        fill=${ACCENT_INK}
      />
      ${sliced
        ? html`<path
            d="M${STEM_L} ${y + SLICE_H} H${STEM_R} V${BASELINE} H${STEM_L} Z"
            fill=${ACCENT_INK}
          />`
        : ''}
    </g>
  `;
}

/**
 * Where the trailing edge crosses the x-height line, in leaned coordinates.
 *
 * This is the number the whole round turns on. It is the leftmost point at
 * which the next letter can sit without running into the flap, so the wordmark
 * is positioned FROM it rather than from a gap chosen by eye. The previous
 * round set the gap by hand off the bowl's nominal right edge and left a hole
 * between the P and the i wide enough to read as a mark parked in front of a
 * name.
 *
 * Returns null when the apex is not above the x-height line, which means the
 * bowl cannot overhang at all and the caller has to fall back to butting the
 * word against the apex.
 */
function trailingEdgeAtXHeight(v: DeltaP, xTop: number): number | null {
  const a = apexOf(v);
  if (a.y >= xTop || v.bowlBottom <= xTop) return null;
  const t = (xTop - a.y) / (v.bowlBottom - a.y);
  const x = a.x - t * (a.x - STEM_R);
  return x - xTop * TAN9;
}

/** A square crop around the leaned letter's own ink. */
function monoViewBox(v: DeltaP): string {
  const a = apexOf(v);
  const right = a.x - a.y * TAN9;
  const left = STEM_L - BASELINE * TAN9;
  const side = Math.max(right - left, BASELINE - CAP_TOP) + 22;
  const cx = (left + right) / 2;
  const cy = (CAP_TOP + BASELINE) / 2;
  return `${(cx - side / 2).toFixed(2)} ${(cy - side / 2).toFixed(2)} ${side.toFixed(2)} ${side.toFixed(2)}`;
}

export function deltaPMark(v: DeltaP, px: number, opts: { sliced?: boolean } = {}) {
  return html`
    <svg
      viewBox=${monoViewBox(v)}
      width=${px}
      height=${px}
      class="block shrink-0"
      aria-hidden="true"
      focusable="false"
    >
      ${deltaPLetter(v, { sliced: opts.sliced ?? false })}
    </svg>
  `;
}

/**
 * The lockup: the same paths, a wider crop, and the rest of the name tucked
 * under the flap.
 *
 * The word is set as a dotless i followed by "lots", because the flap is doing
 * the tittle's job. Its start is derived from the trailing edge rather than
 * chosen, and the arithmetic collapses pleasantly: the word is skewed about
 * the origin and pushed back by baseline times tan(9 degrees) so its baseline
 * lands where asked, and the letter is skewed about the origin with no such
 * correction, so the two lean terms cancel everywhere except that constant.
 * What is left is the crossing point, plus a clearance, minus the shift.
 */
export function deltaPLockup(
  v: DeltaP,
  opts: { height: number; face: 'sans' | 'mono'; sliced: boolean; clearance?: number },
) {
  const fontSize = opts.face === 'mono' ? 126 : 139;
  /** Where the following letters' shoulders start, for this face at this size. */
  const xTop = BASELINE - (opts.face === 'mono' ? 0.55 : 0.52) * fontSize;
  const leanShift = BASELINE * TAN9;
  const clearance = opts.clearance ?? 7;
  const cross = trailingEdgeAtXHeight(v, xTop);
  const a = apexOf(v);
  const tx =
    cross === null
      ? a.x - a.y * TAN9 + clearance - leanShift
      : cross + clearance - leanShift;
  const width = tx + leanShift + (opts.face === 'mono' ? 340 : 278);
  const vbLeft = STEM_L - BASELINE * TAN9 - 11;
  const vbTop = CAP_TOP - 14;
  const vbH = BASELINE - vbTop + 12;
  return html`
    <svg
      viewBox="${vbLeft.toFixed(2)} ${vbTop} ${(width - vbLeft).toFixed(2)} ${vbH}"
      height=${opts.height}
      width=${Math.round((opts.height * (width - vbLeft)) / vbH)}
      role="img"
      aria-label="Pilots"
      class="block"
    >
      ${deltaPLetter(v, { sliced: opts.sliced })}
      <g transform="translate(${leanShift.toFixed(2)} 0) ${LEAN}">
        <text
          x=${tx.toFixed(2)}
          y=${BASELINE}
          font-size=${fontSize}
          class=${opts.face === 'mono' ? 'dp-mono' : 'dp-sans'}
          fill="currentColor"
        >
          ılots
        </text>
      </g>
      ${opts.sliced
        ? html`<rect
            x=${(cross ?? 0).toFixed(2)}
            y=${v.bowlBottom}
            width=${width}
            height=${SLICE_H}
            fill="var(--logo-bg)"
          />`
        : ''}
    </svg>
  `;
}

export const DELTA_PS: DeltaP[] = [
  {
    id: 'rake',
    name: 'Rake',
    idea:
      'The least shear that buys an overhang. The apex lifts just clear of the x-height line, so the i tucks under the flap tip with nothing between them and the letter still sits in a square.',
    cost:
      'The overhang is barely a letter wide, so the interlock reads as tight spacing rather than as two shapes sharing a space.',
    bowlBottom: 66,
    rake: 12,
    reach: 1.15,
    notch: 0.5,
    blunt: 14,
  },
  {
    id: 'rake-deep',
    name: 'Rake, deep',
    idea:
      'The apex lifted well above the midpoint and the flap given more reach. The i sits most of its width under the overhang, which is the arrangement the sibling lockup uses, and the flap plainly stands in for the tittle.',
    cost:
      'Shearing this hard leaves the trailing edge much longer than the leading one. Beside the standalone delta, whose edges converge evenly, it is visibly a different shape.',
    bowlBottom: 66,
    rake: 24,
    reach: 1.4,
    notch: 0.52,
    blunt: 14,
  },
  {
    id: 'rake-sharp',
    name: 'Rake, sharp',
    idea:
      'The deep rake with the blades left at their points where they meet the stem, the way the delta draws its own trailing edges. It is the version that still looks like the mark that was picked.',
    cost:
      'The bowl hangs off two hairlines. It is the first thing to close up at favicon size and the first to fill in when a press spreads the ink.',
    bowlBottom: 66,
    rake: 24,
    reach: 1.4,
    notch: 0.52,
    blunt: 0,
  },
  {
    id: 'rake-long',
    name: 'Rake, long',
    idea:
      'Reach pushed as far as the letter will carry. The flap covers the i completely and reaches the l, which makes the strongest single object of the three words it touches.',
    cost:
      'The letter is now half again as wide as it is tall, so the monogram crop is no longer square and a favicon has to shrink the whole thing to fit the flap in.',
    bowlBottom: 62,
    rake: 26,
    reach: 1.75,
    notch: 0.55,
    blunt: 14,
  },
];
