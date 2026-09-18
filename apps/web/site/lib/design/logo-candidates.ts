import { html } from '@webjsdev/core';

/**
 * The brand mark.
 *
 * This file held six competing drawings while the mark was being chosen. Delta
 * won, so the others are out of the repo rather than left lying around: a
 * rejected mark that stays in the tree gets rendered by accident eventually,
 * and a losing drawing kept "for reference" is the thing somebody reaches for
 * when they cannot find the real one.
 *
 * THE THREE RULES THE DRAWING OBEYS, which are why it survives being scaled
 * and inverted:
 *
 * 1. It is authored on a 32-unit grid, which is the favicon grid. Designing at
 *    hero size and shrinking is how a mark ends up with a detail that only
 *    exists above 64 pixels. This one was drawn small first.
 * 2. Ink is `currentColor` and paper is `var(--logo-bg)`. That is what lets one
 *    drawing sit on a dark tile, a light tile and the live page without a
 *    second file, and it is why the band really is negative space rather than a
 *    hard-coded near-black that goes wrong on paper.
 * 3. One part reads `var(--logo-accent, …)`, so a container that sets
 *    `--logo-accent` gets the acid-green variant and every other container gets
 *    the monochrome one. No branching, no second drawing.
 */
export type Candidate = {
  id: string;
  name: string;
  /** The single idea the drawing carries. */
  idea: string;
  /** What it costs. Every drawing trades something away. */
  cost: string;
  /** The artwork, on a 32-unit grid. A function, so each render is its own nodes. */
  art: () => unknown;
};

/** Ink that turns acid green when the container sets `--logo-accent`. */
const ACCENT_INK = 'var(--logo-accent, currentColor)';

/**
 * Delta: the mark.
 *
 * The band across it is the load-bearing part. An upward triangle on its own
 * sits close to a play button, and the cut plus the forward lean are what hold
 * the two apart. Nothing may reshape either.
 */
export const DELTA: Candidate = {
  id: 'delta',
  name: 'Delta',
  idea: 'The aircraft symbol off a moving map, swept back and leaning forward. It carries motion in the drawing itself, and the band cut through it is the one feature no other triangle has.',
  cost: 'An upward triangle sits close to a play button, and the lean and the cut are the only things holding the two apart. Neither may be dropped at small sizes.',
  art: () => html`
    <!-- skewX(-11) leans the nose forward. The translate re-centres what the
         skew pushed left: tan(11 degrees) * 16 is about 3.1, and the shape
         spans a further 2.3 to the left of centre, so 5.4 puts the drawn
         bounds back on symmetric margins. -->
    <g transform="translate(5.4 0)">
      <g transform="skewX(-11)">
        <path d="M16 3.8 L26.8 27.6 L16 21.4 L5.2 27.6 Z" fill=${ACCENT_INK} />
      </g>
    </g>
    <rect x="3" y="17.6" width="26" height="2" fill="var(--logo-bg)" />
  `,
};

/** The mark at a given pixel size, on whatever paper the container declares. */
export function markSvg(c: Candidate, px: number) {
  return html`
    <svg
      viewBox="0 0 32 32"
      width=${px}
      height=${px}
      class="block shrink-0"
      aria-hidden="true"
      focusable="false"
    >
      ${c.art()}
    </svg>
  `;
}

/**
 * The mark as the site ships it, in the header and the footer.
 *
 * One function, so the live chrome and the brand page render the same drawing
 * rather than two copies that drift. `--logo-bg` is the paper it sits on: the
 * drawing cuts its band out with that colour, so a container on a different
 * surface must declare it or the cut shows as a bar of the wrong tone.
 */
export function brandMark(px: number) {
  return markSvg(DELTA, px);
}
