import { html } from '@webjsdev/core';

/**
 * Delta, fused into a P, driving the whole word.
 *
 * The brief is the sibling brand's construction rather than its drawing. On
 * webjs.dev the monogram is not a badge parked beside the name, it IS the W
 * of WebJs, the wordmark leans with it, and one slice cuts monogram and word
 * together so the lockup reads as a single object. Everything below is that
 * idea applied to the delta that came out of the candidate round.
 *
 * FOUR THINGS THIS SET HOLDS CONSTANT, so the variants differ on the thing
 * being decided rather than on everything at once:
 *
 * 1. The lean is skewX(-9) everywhere, monogram and wordmark alike. The
 *    delta's whole argument was that it carries motion, and a leaning mark
 *    beside an upright word throws that away.
 * 2. The letter's baseline is declared per variant, so the lockup can seat
 *    the drawing on the same baseline as the type instead of centring a
 *    square box next to a line of text, which is what makes a mark sit
 *    visibly too high.
 * 3. The word is set as live SVG text on that baseline. A shipped asset would
 *    be re-cut as outlines (the sibling brand carries the same note, having
 *    shipped a 56 KB embedded font first and outlined it later), but for
 *    deciding a shape, live text is the version you can actually change.
 * 4. The slice is painted in `--logo-bg` rather than cut out of the geometry.
 *    In the shipped file it has to become a real gap, the way webjs draws its
 *    W as two separate paths with air between them. Painted, it is correct on
 *    any surface whose background the container declares, which every surface
 *    in this lab does.
 */
export type Fusion = {
  id: string;
  name: string;
  idea: string;
  cost: string;
  /**
   * The LETTER only, on a 32-unit grid, already leaned and centred.
   *
   * The slice is deliberately not in here. The lockup paints one band across
   * the mark and the word together, and a mark carrying its own band would
   * put a second cut at a different height inside the first.
   */
  art: () => unknown;
  /** The slice band for the mark shown on its own, in grid units. */
  monoSlice: { y: number; h: number };
  /** Where the letter sits down on its baseline, in grid units. */
  baseline: number;
  /** The letter this mark supplies, so the wordmark can drop it. */
  initial: string;
  /**
   * Lockup placement. Tuned against the render rather than derived, because
   * the miter on a sharp apex puts ink well outside the path's own points and
   * no amount of arithmetic on the control points predicts where.
   */
  lockup: {
    /** Scale applied to the 32-unit drawing inside the lockup box. */
    scale: number;
    /** Horizontal offset of the drawing. */
    dx: number;
    /** Where the wordmark starts. Overlaps the mark on purpose where it can. */
    tx: number;
    /** Type size for the wordmark, chosen so its height matches the mark's. */
    fontSize: number;
    /** Centre of the slice band. */
    sliceY: number;
  };
};

/** Ink that turns acid green when the container sets `--logo-accent`. */
const ACCENT_INK = 'var(--logo-accent, currentColor)';

/** The lean, shared by every mark and every wordmark here. */
const LEAN = 'skewX(-9)';

export const FUSIONS: Fusion[] = [
  /*
   * WHAT THE FIRST ROUND RULED OUT, because it is the useful half of the
   * result and the drawings themselves are gone.
   *
   * The obvious fusion is to rotate the delta to point forward and weld it to
   * a stem, so the arrowhead becomes the bowl. Three versions of that were
   * drawn, solid and stroked and lowercase, and all three read as a FLAG ON A
   * POLE rather than as a letter. The reason is structural and no amount of
   * tuning the angle fixes it: a P's bowl leaves the stem and RETURNS to it,
   * and a triangle whose point aims away from the stem never comes back. The
   * eye sees a pennant.
   *
   * So the surviving idea is the opposite one. Keep the letter's silhouette
   * honest and put the delta INSIDE it, as the counter, where a triangle is
   * enclosed by definition. The variants below differ in which way that
   * triangle points and in how the letter carrying it is built.
   */
  {
    id: 'counter-up',
    name: 'Counter, up',
    initial: 'P',
    baseline: 28.4,
    idea:
      'An ordinary P, leaning, with the delta cut out as its counter. The letter stays a letter and the delta stays a delta, because neither is being asked to be the other.',
    cost:
      'The delta points up while the letter leans forward, so the mark holds two directions at once and neither one wins.',
    art: () => html`
      <g transform="translate(3 0)">
        <g transform=${LEAN}>
          <path d="M7 4.2 H20.6 a7.1 7.1 0 0 1 0 14.2 H11.6 V28.4 H7 Z" fill=${ACCENT_INK} />
          <path d="M18.1 7.8 L22.6 15 H13.6 Z" fill="var(--logo-bg)" />
        </g>
      </g>
    `,
    monoSlice: { y: 21, h: 1.9 },
    lockup: { scale: 1, dx: -4, tx: 26.5, fontSize: 33.6, sliceY: 18.6 },
  },
  {
    id: 'counter-fwd',
    name: 'Counter, forward',
    initial: 'P',
    baseline: 28.4,
    idea:
      'The same letter with the counter turned to point the way the letter leans. It is the version that agrees with itself, and it puts the arrowhead back on the heading it had as a standalone mark.',
    cost:
      'A right-pointing triangle inside a bowl is close to the play button the standalone delta was already accused of being, and here it is boxed in, which is exactly how a play button is drawn.',
    art: () => html`
      <g transform="translate(3 0)">
        <g transform=${LEAN}>
          <path d="M7 4.2 H20.6 a7.1 7.1 0 0 1 0 14.2 H11.6 V28.4 H7 Z" fill=${ACCENT_INK} />
          <path d="M22.8 11.3 L14.2 15.6 V7 Z" fill="var(--logo-bg)" />
        </g>
      </g>
    `,
    monoSlice: { y: 21, h: 1.9 },
    lockup: { scale: 1, dx: -4, tx: 26.5, fontSize: 33.6, sliceY: 18.6 },
  },
  {
    id: 'swept',
    name: 'Swept',
    initial: 'P',
    baseline: 28.4,
    idea:
      'One constant-width stroke, the way the sibling brand draws its W. The bowl squares off at the right so the letter reads, then the underside runs back to the stem as one long diagonal, which is the delta trailing edge doing the work instead of a triangle sitting in a hole.',
    cost:
      'The delta is implied rather than drawn. Nobody who has not seen the arrowhead version will find an aircraft in this, and it gives up the one shape the round was supposed to keep.',
    art: () => html`
      <g transform="translate(3.82 0)">
        <g transform=${LEAN}>
          <path
            d="M9.3 28.4 V5.4 H21.3 V11 L9.3 19.6"
            fill="none"
            stroke=${ACCENT_INK}
            stroke-width="4.6"
            stroke-linejoin="miter"
            stroke-linecap="butt"
            stroke-miterlimit="3"
          />
        </g>
      </g>
    `,
    monoSlice: { y: 15.3, h: 2 },
    lockup: { scale: 1, dx: -3.8, tx: 25, fontSize: 33.6, sliceY: 17.9 },
  },
  {
    id: 'counter-lower',
    name: 'Counter, lowercase',
    initial: 'p',
    baseline: 22,
    idea:
      'The counter idea as a lowercase p, bowl at x-height with a descender under it. The only variant that leaves the name set the way the site sets it on every page today.',
    cost:
      'The descender has to go somewhere. In a square placement it either crowds the tile or shrinks the whole letter, so this is the variant that pays the most for a favicon.',
    art: () => html`
      <g transform="translate(3.2 0)">
        <g transform=${LEAN}>
          <path d="M7 9.2 H18 a6.4 6.4 0 0 1 0 12.8 H11.6 V29.5 H7 Z" fill=${ACCENT_INK} />
          <path d="M17 12.2 L20.4 18.6 H13.6 Z" fill="var(--logo-bg)" />
        </g>
      </g>
    `,
    monoSlice: { y: 24.4, h: 1.9 },
    lockup: { scale: 1.28, dx: -5, tx: 30, fontSize: 31.5, sliceY: 26.6 },
  },
];

/** One fusion monogram at one size. */
export function fusionMark(f: Fusion, px: number) {
  return html`
    <svg
      viewBox="0 0 32 32"
      width=${px}
      height=${px}
      class="block shrink-0"
      aria-hidden="true"
      focusable="false"
    >
      ${f.art()}
      <rect x="2" y=${f.monoSlice.y} width="28" height=${f.monoSlice.h} fill="var(--logo-bg)" />
    </svg>
  `;
}

/**
 * The full lockup: the mark supplying the initial, the rest of the word set
 * on the same baseline, and optionally the slice running through both.
 *
 * The word is passed WITHOUT its first letter. That is the whole construction
 * being tested: if the wordmark still spells the name with the mark removed,
 * the mark is a badge and not a monogram.
 */
export function fusionLockup(
  f: Fusion,
  opts: { height: number; face: 'sans' | 'mono'; slice: boolean; lean: boolean },
) {
  const { scale, dx, tx, fontSize, sliceY } = f.lockup;
  const BASE = 30;
  const dy = BASE - f.baseline * scale;
  const rest = 'pilots'.slice(1);
  /* The wordmark leans on the same axis as the mark. Skewing about the origin
     drags the text left by baseline * tan(9 degrees), so the start is pushed
     back by that much to land where tx asks for. */
  const leanShift = opts.lean ? BASE * 0.15838 : 0;
  return html`
    <svg
      viewBox="0 0 120 40"
      height=${opts.height}
      width=${Math.round((opts.height * 120) / 40)}
      role="img"
      aria-label="pilots"
      class="block"
    >
      <g transform="translate(${dx} ${dy}) scale(${scale})">${f.art()}</g>
      <g transform=${opts.lean ? `translate(${leanShift} 0) ${LEAN}` : ''}>
        <text
          x=${tx}
          y=${BASE}
          font-size=${fontSize}
          class=${opts.face === 'mono' ? 'lockup-mono' : 'lockup-sans'}
          fill="currentColor"
        >
          ${rest}
        </text>
      </g>
      ${opts.slice
        ? html`<rect
            x="0"
            y=${sliceY - 1.1}
            width="120"
            height="2.2"
            fill="var(--logo-bg)"
          />`
        : ''}
    </svg>
  `;
}
