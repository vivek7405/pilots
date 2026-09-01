import { html } from '@webjsdev/core';

/**
 * The delta, turned onto a heading and made into the P of pilots.
 *
 * ONE DRAWING, TWO CROPS. That is the whole construction, and it is lifted
 * from the sibling brand rather than invented: webjs.dev's standalone mark and
 * its lockup are the same two path elements with identical coordinates, and
 * the only difference between the files is the viewBox. So the monogram is not
 * a version of the logo, it is the logo with the word cropped off, and the two
 * cannot drift because there is nothing to keep in sync.
 *
 * The letter is drawn in one coordinate space, described here once:
 *
 *   cap top     y = 8         baseline    y = 108      (cap height 100)
 *   stem        x = 18 to 44                           (26 wide, 0.26 of cap)
 *   bowl        y = 8 to BOWL_BOTTOM, apex at x = 128
 *   slice       y = 68 to 77
 *
 * The stem is 0.22 of the cap height because that is where a black weight sits.
 * The previous round drew a heavier letter and it read as an object sitting
 * next to type rather than as the first letter of the word.
 *
 * THE SLICE IS A REAL GAP, not a painted bar. The stem is emitted as two
 * separate paths with air between them, which is how the sibling brand draws
 * its W, and it is what lets one file sit on any background. It only works
 * because the slice crosses the stem alone: the bowl stops above it, so no
 * polygon has to be split.
 */

const ACCENT_INK = 'var(--logo-accent, currentColor)';

const CAP_TOP = 8;
const BASELINE = 108;
const STEM_L = 18;
const STEM_R = 44;
const SLICE_H = 9;

/**
 * The delta's own aspect: its length divided by its wingtip span, taken off the
 * candidate drawing (23.8 over 21.6). The mark is very nearly square.
 *
 * The bowl's length is DERIVED from its height through this number rather than
 * set by hand, and that is the correction that made this round work. The first
 * attempt fixed the apex at a constant x, which stretched the rotated delta to
 * roughly two to one and turned every variant back into a pennant. A bowl only
 * reads as a bowl when it is about as deep as it is tall.
 */
const DELTA_ASPECT = 23.8 / 21.6;

/** Where the blades converge, derived so the delta keeps its proportions. */
function apexX(bowlBottom: number) {
  return STEM_R + (bowlBottom - CAP_TOP) * DELTA_ASPECT;
}

/** The lean, on the letter and on the word alike. */
const LEAN = 'skewX(-9)';

export type DeltaP = {
  id: string;
  name: string;
  idea: string;
  cost: string;
  /** How far the delta's tail notch cuts back, as a fraction of the bowl's length. */
  notch: number;
  /**
   * Vertical thickness where each blade meets the stem.
   *
   * Zero is the delta's own geometry, whose trailing edges converge to sharp
   * points. A letter's bowl normally joins its stem at full stroke weight, so
   * this is the dial between keeping the aircraft and keeping the P.
   */
  blunt: number;
  /** Where the bowl closes on the stem. */
  bowlBottom: number;
};

export const DELTA_PS: DeltaP[] = [
  {
    id: 'blade',
    name: 'Blade',
    idea:
      'The delta rotated onto a heading and butted against a stem, keeping its own proportions. The tail notch becomes the counter, so the letter is built from the mark rather than around it.',
    cost:
      'The notch is shallow because the delta drew it that way, so the counter is a nick rather than a hole and the bowl reads as one solid mass.',
    notch: 0.28,
    blunt: 0,
    bowlBottom: 70,
  },
  {
    id: 'counter',
    name: 'Counter',
    idea:
      'The same construction with the notch cut back until the void behind the blades is a real counter. This is the least a P can enclose and still be read as one.',
    cost:
      'Both blades taper to a point where they meet the stem, so the bowl hangs off two hairlines. It is the first thing to break at small size and in a stroke-thickening print.',
    notch: 0.5,
    blunt: 0,
    bowlBottom: 70,
  },
  {
    id: 'joined',
    name: 'Joined',
    idea:
      'The counter version with the blades given real thickness where they land on the stem. A bowl that meets its stem at full weight is what separates a letter from a shape stuck to a bar.',
    cost:
      'Blunting the tips is the one place this stops being the delta. The trailing edges no longer converge, which is the detail that made the standalone mark look like it was moving.',
    notch: 0.5,
    blunt: 16,
    bowlBottom: 70,
  },
  {
    id: 'deep',
    name: 'Deep',
    idea:
      'Joined, with a longer bowl and a deeper notch. The counter opens up and the blades sweep further, which is the version that reads as a letter from furthest away.',
    cost:
      'The bowl runs past the halfway line of the cap, so the stem below it is short and the letter sits closer to a D than a P.',
    notch: 0.6,
    blunt: 18,
    bowlBottom: 76,
  },
];

/**
 * The bowl outline.
 *
 * Traced clockwise from the top join: out along the leading edge to the apex,
 * back along the trailing edge to the bottom join, then in and out of the
 * notch. With `blunt` at zero the two stem-side vertices collapse onto the
 * joins and the shape is the delta exactly.
 */
function bowlPath(v: DeltaP): string {
  const apexY = (CAP_TOP + v.bowlBottom) / 2;
  const ax = apexX(v.bowlBottom);
  const notchX = STEM_R + (ax - STEM_R) * v.notch;
  const b = v.blunt;
  return [
    `M${STEM_R} ${CAP_TOP}`,
    `L${ax} ${apexY}`,
    `L${STEM_R} ${v.bowlBottom}`,
    `L${STEM_R} ${v.bowlBottom - b}`,
    `L${notchX} ${apexY}`,
    `L${STEM_R} ${CAP_TOP + b}`,
    'Z',
  ].join(' ');
}

/**
 * The letter, as separate paths.
 *
 * Three of them: the bowl, the stem above the slice, and the stem below it.
 * The gap between the last two IS the slice. Emitting it as absent geometry
 * rather than as a bar in the background colour is what makes one file correct
 * on ink, on paper, and on a photograph.
 */
export function deltaPLetter(v: DeltaP, opts: { sliced: boolean } = { sliced: true }) {
  /* The cut starts exactly where the bowl closes, so the upper piece is bowl
     plus stem and the lower piece is the rest of the stem. Putting it anywhere
     else would mean splitting the bowl polygon, and a slice that has to cut a
     concave shape in two is a slice that will be redrawn by hand every time
     the bowl moves. */
  const sliceY = v.bowlBottom;
  const stemTop = opts.sliced
    ? `M${STEM_L} ${CAP_TOP} H${STEM_R} V${sliceY} H${STEM_L} Z`
    : `M${STEM_L} ${CAP_TOP} H${STEM_R} V${BASELINE} H${STEM_L} Z`;
  return html`
    <g transform=${LEAN}>
      <path d=${bowlPath(v)} fill=${ACCENT_INK} />
      <path d=${stemTop} fill=${ACCENT_INK} />
      ${opts.sliced
        ? html`<path
            d="M${STEM_L} ${sliceY + SLICE_H} H${STEM_R} V${BASELINE} H${STEM_L} Z"
            fill=${ACCENT_INK}
          />`
        : ''}
    </g>
  `;
}

/**
 * The monogram: the letter with the word cropped off.
 *
 * The box is square and centred on the leaned letter's ink, which lands the
 * mark off-centre in its own bounding box. That is correct and deliberate: the
 * lean throws the stem's foot left and the apex right, so matching the box
 * margins would push the letter's weight up and to the left. The sibling
 * brand's monogram carries the same note for the same reason.
 */
const MONO_VB = '-10.5 -6 128 128';

export function deltaPMark(v: DeltaP, px: number, opts: { sliced?: boolean } = {}) {
  return html`
    <svg
      viewBox=${MONO_VB}
      width=${px}
      height=${px}
      class="block shrink-0"
      aria-hidden="true"
      focusable="false"
    >
      ${deltaPLetter(v, { sliced: opts.sliced ?? true })}
    </svg>
  `;
}

/**
 * The lockup: the same paths, a wider crop, and the rest of the name.
 *
 * The word is passed without its first letter. The type is set at the size
 * that puts its cap height on the drawn letter's cap height, so the P is the
 * word's first letter rather than a mark parked in front of it.
 *
 * The band across the word is painted, not cut, and that is a limit of using
 * live text: glyph outlines cannot be split without outlining the face first.
 * A shipped file outlines the word and the slice becomes a gap there too.
 */
const LOCKUP_VB_H = 128;

export function deltaPLockup(
  v: DeltaP,
  opts: { height: number; face: 'sans' | 'mono'; sliced: boolean; leanWord: boolean },
) {
  const fontSize = opts.face === 'mono' ? 126 : 139;
  /* The word starts just past where the leaned apex actually lands, not past
     the bowl's nominal right edge. The lean pulls the apex back by its own
     height times tan(9 degrees), and setting the gap off the un-leaned number
     opened a hole between the letter and the word wide enough to read as a
     mark parked in front of a name. */
  const apexY = (CAP_TOP + v.bowlBottom) / 2;
  const apexInk = apexX(v.bowlBottom) - apexY * 0.15838;
  const tx = apexInk + (opts.face === 'mono' ? 16 : 10);
  const width = tx + (opts.face === 'mono' ? 330 : 268);
  const vb = `-10.5 -6 ${width} ${LOCKUP_VB_H}`;
  /* Skewing about the origin drags the word left by baseline * tan(9 degrees),
     so the run is pushed back by that much to start where tx asks. */
  const leanShift = opts.leanWord ? BASELINE * 0.15838 : 0;
  return html`
    <svg
      viewBox=${vb}
      height=${opts.height}
      width=${Math.round((opts.height * width) / LOCKUP_VB_H)}
      role="img"
      aria-label="Pilots"
      class="block"
    >
      ${deltaPLetter(v, { sliced: opts.sliced })}
      <g transform=${opts.leanWord ? `translate(${leanShift} 0) ${LEAN}` : ''}>
        <text
          x=${tx}
          y=${BASELINE}
          font-size=${fontSize}
          class=${opts.face === 'mono' ? 'dp-mono' : 'dp-sans'}
          fill="currentColor"
        >
          ilots
        </text>
      </g>
      ${opts.sliced
        ? html`<rect
            x=${tx - 12}
            y=${v.bowlBottom}
            width=${width}
            height=${SLICE_H}
            fill="var(--logo-bg)"
          />`
        : ''}
    </svg>
  `;
}
