import { html } from '@webjsdev/core';

/**
 * The delta sitting over the word, with a stem cut from its own partition.
 *
 * THE MARK IS NEVER REDRAWN. Every variant here is the candidate path, with
 * its own lean and its own tail notch, only moved, turned, and scaled. Earlier
 * rounds sheared and blunted it until it was a different shape. Nothing below
 * touches the geometry.
 *
 * WHY THE TURN IS NOT EXACTLY NINETY DEGREES, which is the one piece of
 * arithmetic worth reading here.
 *
 * The wing has to clear two letters of very different heights: the dotless i,
 * whose shoulder sits a third of the way down the cap, and the l, an ascender
 * reaching almost to the top. Its underside therefore has to climb from just
 * above the i to just above the l across a single letter's width, and that is
 * a slope of roughly forty-three degrees.
 *
 * The delta's trailing edge sits at 24.4 degrees to its own axis, that being
 * atan of half its span over its length. Turned exactly ninety degrees the
 * axis is horizontal, so the underside climbs at 24.4 degrees, which is not
 * enough: set it clear of the i and it cuts straight through the l. Turning it
 * a further nineteen degrees puts the underside on the slope the word actually
 * makes, and the wing then follows the letters instead of ignoring them. That
 * is why `rotate` is a dial rather than a constant.
 *
 * THE STEM IS CUT FROM THE PARTITION. The delta already carries a slice across
 * it, and turning the mark stands that slice up. Its position is carried
 * through the same transform as everything else, so the stem drops from
 * wherever the mark's own cut has landed rather than from a coordinate picked
 * by hand. It is drawn at the weight of the surrounding letters rather than as
 * a slab, so the lockup reads as one piece of lettering.
 */

const ACCENT_INK = 'var(--logo-accent, currentColor)';

const BASELINE = 108;
const DESCENDER = 132;

/** The candidate drawing, verbatim, inside the translate and lean it was drawn with. */
const DELTA_D = 'M16 3.8 L26.8 27.6 L16 21.4 L5.2 27.6 Z';
/** Centre of its ink box in its own 32-unit grid. */
const D_CX = 16.035;
const D_CY = 15.7;
/** The partition's offset from that centre: the slice runs y 17.6 to 19.6. */
const PARTITION_DY = 18.6 - D_CY;

const RAD = Math.PI / 180;

export type DeltaP = {
  id: string;
  name: string;
  idea: string;
  cost: string;
  /** Degrees clockwise. Ninety is a plain quarter turn, more rakes the nose up. */
  rotate: number;
  /** Scale applied to the 32-unit drawing. */
  size: number;
  /** Where the ink box's centre lands. */
  cx: number;
  cy: number;
  /** Stem weight, matched to the letters rather than to the mark. */
  stemW: number;
  stem: 'none' | 'baseline' | 'descender';
  /**
   * Carry the Delta half drawing: the hairline opened along the mark's own
   * axis. Identical geometry to the candidate card, cut and all.
   */
  halved?: boolean;
  /**
   * Where the stem hangs, when the mark's partition cannot say.
   *
   * The halved drawing has no crossways partition left to hang from, its cut
   * running along the axis instead, so that variant states the position rather
   * than deriving one.
   */
  stemX?: number;
  /** Where the stem starts, when the underside is not the edge it meets. */
  stemTop?: number;
};

/** The delta, placed. Its ink centre lands on (cx, cy) whatever the rotation. */
function deltaAt(v: DeltaP) {
  return html`
    <g
      transform="translate(${v.cx} ${v.cy}) scale(${v.size}) rotate(${v.rotate}) translate(${-D_CX} ${-D_CY})"
    >
      <g transform="translate(5.4 0)">
        <g transform="skewX(-11)">
          <path d=${DELTA_D} fill=${ACCENT_INK} />
          <!-- The Delta half cut, verbatim from the candidate card: a hairline
               on x = 16, the line the delta is symmetric about before it is
               leaned. Inside the skew, so it leans with the drawing and runs
               tip to tail rather than crossing it. -->
          ${v.halved
            ? html`<rect x="15.7" y="2" width="0.6" height="28" fill="var(--logo-bg)" />`
            : ''}
        </g>
      </g>
    </g>
  `;
}

/**
 * Where the mark's own partition has ended up.
 *
 * The slice sits on the drawing's centre line, PARTITION_DY below the ink
 * centre, so carrying it through the transform is a rotation of that single
 * offset. The stem hangs from here, which is what makes it the mark's stem
 * rather than a bar that happens to stand nearby.
 */
export function partitionX(v: DeltaP) {
  return v.cx - v.size * PARTITION_DY * Math.sin(v.rotate * RAD);
}

/** A vertex of the leaned drawing, carried through the variant's transform. */
function pt(v: DeltaP, lx: number, ly: number) {
  const dx = lx - D_CX;
  const dy = ly - D_CY;
  const c = Math.cos(v.rotate * RAD);
  const s = Math.sin(v.rotate * RAD);
  return { x: v.cx + v.size * (dx * c - dy * s), y: v.cy + v.size * (dx * s + dy * c) };
}

/**
 * Where the wing's underside sits at a given x, so the stem can start there.
 *
 * WHICH edge is the underside depends on the turn, so it is found rather than
 * assumed. Getting this wrong is what produced a mark hovering uselessly above
 * the word: the lean means the drawing's own axis is already about twenty
 * degrees off vertical, so reasoning about the turn from the upright shape
 * gives an angle that is wrong by that much and in the wrong direction.
 */
function undersideY(v: DeltaP, x: number): number {
  const apex = pt(v, 20.661, 3.8);
  const wings = [pt(v, 5.235, 27.6), pt(v, 26.835, 27.6)];
  const tip = wings[0].y > wings[1].y ? wings[0] : wings[1];
  const t = (x - tip.x) / (apex.x - tip.x);
  return tip.y + t * (apex.y - tip.y);
}

/** The mark: the delta, and the thin stem dropped from its partition. */
export function deltaPLetter(v: DeltaP) {
  const px = v.stemX ?? partitionX(v);
  const top = v.stemTop ?? undersideY(v, px) - 2;
  const bottom = v.stem === 'descender' ? DESCENDER : BASELINE;
  return html`
    ${v.stem === 'none'
      ? ''
      : html`<g transform="skewX(-9)">
          <path
            d="M${(px - v.stemW / 2).toFixed(1)} ${top.toFixed(1)} h${v.stemW} V${bottom} h${-v.stemW} Z"
            fill=${ACCENT_INK}
          />
        </g>`}
    ${deltaAt(v)}
  `;
}

/** Ink bounds of the mark alone, for the monogram crop. */
function markBox(v: DeltaP) {
  const pts = [
    pt(v, 20.661, 3.8),
    pt(v, 26.835, 27.6),
    pt(v, 17.24, 21.4),
    pt(v, 5.235, 27.6),
  ];
  const xs = pts.map((p) => p.x);
  const ys = pts.map((p) => p.y);
  const stemLean = BASELINE * 0.15838;
  if (v.stem !== 'none') {
    const px = v.stemX ?? partitionX(v);
    xs.push(px - v.stemW / 2 - stemLean, px + v.stemW / 2);
    ys.push(v.stem === 'descender' ? DESCENDER : BASELINE);
  }
  const pad = 10;
  const left = Math.min(...xs) - pad;
  const top = Math.min(...ys) - pad;
  return { left, top, w: Math.max(...xs) - left + pad, h: Math.max(...ys) - top + pad };
}

export function deltaPMark(v: DeltaP, px: number) {
  const b = markBox(v);
  return html`
    <svg
      viewBox="${b.left.toFixed(1)} ${b.top.toFixed(1)} ${b.w.toFixed(1)} ${b.h.toFixed(1)}"
      height=${px}
      width=${Math.round((px * b.w) / b.h)}
      class="block shrink-0"
      aria-hidden="true"
      focusable="false"
    >
      ${deltaPLetter(v)}
    </svg>
  `;
}

/**
 * The lockup.
 *
 * The word sits at ONE position in every variant, at ordinary spacing off the
 * stem, and the delta is what moves. Positioning the word to clear the mark is
 * what opened the gap between the letter and the i in every earlier round.
 */
const FONT_SIZE = 139;
const WORD_X = 50;

export function deltaPLockup(v: DeltaP, opts: { height: number; face?: 'sans' | 'mono' }) {
  const face = opts.face ?? 'sans';
  const fontSize = face === 'mono' ? 126 : FONT_SIZE;
  const leanShift = BASELINE * 0.15838;
  const b = markBox(v);
  const left = Math.min(b.left, WORD_X - 14);
  const top = Math.min(b.top, -12);
  const bottom = Math.max(b.top + b.h, v.stem === 'descender' ? DESCENDER + 10 : BASELINE + 14);
  const right = Math.max(b.left + b.w, WORD_X + leanShift + (face === 'mono' ? 350 : 292));
  const h = bottom - top;
  return html`
    <svg
      viewBox="${left.toFixed(1)} ${top.toFixed(1)} ${(right - left).toFixed(1)} ${h.toFixed(1)}"
      height=${opts.height}
      width=${Math.round((opts.height * (right - left)) / h)}
      role="img"
      aria-label="pilots"
      class="block"
    >
      <g transform="translate(${leanShift.toFixed(2)} 0) skewX(-9)">
        <text
          x=${WORD_X}
          y=${BASELINE}
          font-size=${fontSize}
          class=${face === 'mono' ? 'dp-mono' : 'dp-sans'}
          fill="currentColor"
        >
          ılots
        </text>
      </g>
      ${deltaPLetter(v)}
    </svg>
  `;
}

export const DELTA_PS: DeltaP[] = [
  {
    id: 'turn-90',
    name: 'Quarter turn',
    idea:
      'The plain quarter turn, kept as the control. Everything else on this row is a departure from it, and this is what they are departing from.',
    cost:
      'Its underside runs at fifteen degrees while the step from the i\'s shoulder to the l\'s ascender is about forty. Lift it clear of the l and it floats far above the i, so it can sit properly over one letter or the other, never both.',
    rotate: 90,
    size: 3.5,
    cx: 60,
    cy: -14,
    stemW: 15,
    stem: 'baseline',
  },
  {
    id: 'raked',
    name: 'Turned to the letters',
    idea:
      'The same drawing turned to sixty-four degrees, which is where its underside lands on the slope the word actually makes from the i up to the l. The wing then sits over both, following their tops, and the stem drops from the mark\'s own partition at the weight of the letters around it.',
    cost:
      'It is a long mark. Cropped square for a favicon the wing has to shrink until the stem is a thread, so a small placement needs the tighter of the two crops rather than this one.',
    rotate: 64,
    size: 5.2,
    cx: 50,
    cy: -12.5,
    stemW: 15,
    stem: 'baseline',
  },
  {
    id: 'raked-half',
    name: 'Turned to the letters half',
    idea:
      'Turned to the letters with the plane opened along its axis, and nothing else altered. Same sixty-four degree turn, same size, same placement, same stem. The only difference on the card is the hairline.',
    cost:
      'The cut is not level here. Sixty-four degrees is the angle that puts the wing on the word, and seventy-nine is the one that lays the cut flat, so this carries the gap about fifteen degrees off horizontal. The two angles cannot both be had.',
    rotate: 64,
    size: 5.2,
    cx: 50,
    cy: -12.5,
    stemW: 15,
    stem: 'baseline',
    halved: true,
  },
  {
    id: 'raked-bare',
    name: 'Turned, no stem',
    idea:
      'The same wing with no stem at all, which is the test that was asked for. The only verticals left under it are the i and the l.',
    cost:
      'Nothing comes down from the mark, so there is no P. It reads as a wing resting on the word rather than as the word\'s first letter.',
    rotate: 64,
    size: 5.2,
    cx: 50,
    cy: -12.5,
    stemW: 15,
    stem: 'none',
  },
  {
    id: 'half-raked',
    name: 'Delta half, over the letters',
    idea:
      'The Delta half drawing from the candidate round, unchanged, with a stem added and nothing else. It keeps its own seventy-nine degree turn, which is the angle that lays its cut flat, so the hairline stays level with the baseline instead of tilting to follow the wing.',
    cost:
      'Seventy-nine degrees is not the angle the word wants. The underside runs at twenty-five degrees against the forty the step from the i up to the l asks for, so setting the wing clear of the l leaves it riding well above the i. That gap is the price of keeping the cut flat.',
    rotate: 79,
    size: 4.2,
    cx: 56,
    cy: -8,
    stemW: 15,
    stem: 'baseline',
    halved: true,
    stemX: 40,
    stemTop: 30,
  },
  {
    id: 'raked-descender',
    name: 'Turned, dropped',
    idea:
      'The stem carried below the baseline, which makes the letter a lowercase p. That is how the name is set on every page of the site, and the descender is the counterweight to a wing that sits high.',
    cost:
      'A descender needs clearance, so any line of type under the lockup has to move down to make room for one letter.',
    rotate: 64,
    size: 5.2,
    cx: 50,
    cy: -12.5,
    stemW: 15,
    stem: 'descender',
  },
];
