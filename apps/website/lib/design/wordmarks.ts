import { html } from '@webjsdev/core';

/**
 * Whole-word treatments that keep the delta and leave the word alone.
 *
 * WHY THE PREVIOUS ROUND WAS SCRAPPED, because it is the constraint this one
 * is built against. That round fused the delta into a P and let the mark
 * supply the word's first letter, which is the construction the sibling brand
 * uses. Four versions shipped to the page and none of them held up. The
 * failure was not any single drawing:
 *
 *   - A hand-drawn letter sits next to font-drawn letters, and no amount of
 *     matching the weight hides that one of them was made by a different
 *     process. The eye reads the P as an object and the rest as type.
 *   - The slice through live text reads as a strikethrough rather than as a
 *     cut, because a strikethrough is what a horizontal bar through a word
 *     already means.
 *   - Making the mark an uppercase P forced the name to "Pilots" when the
 *     site sets it lowercase on every page it has.
 *
 * So the rule here is the opposite one. The delta stays the delta, the word
 * stays a word set in one face, and the two are related by ARRANGEMENT rather
 * than by welding one into the other. Everything below is lowercase.
 */

/** Ink that turns acid green when the container sets `--logo-accent`. */
const ACCENT_INK = 'var(--logo-accent, currentColor)';

/**
 * The delta, exactly as it was drawn in the candidate round: the same path,
 * the same skewX(-11) lean, unsliced. It is imported rather than redrawn
 * because a mark that gets retyped per placement stops being one mark.
 */
const DELTA_D = 'M16 3.8 L26.8 27.6 L16 21.4 L5.2 27.6 Z';
/** The ink box the path actually occupies once leaned and re-centred. */
const D_X = 5.23;
const D_Y = 3.8;
const D_W = 21.61;
const D_H = 23.8;
/** Centre of that ink box, which is what the heading version rotates about. */
const D_CX = D_X + D_W / 2;
const D_CY = D_Y + D_H / 2;

const deltaShape = () => html`
  <g transform="translate(5.4 0)">
    <g transform="skewX(-11)"><path d=${DELTA_D} fill=${ACCENT_INK} /></g>
  </g>
`;

/** The delta pointing up, its ink box placed at (x, y) with height h. */
export function deltaUp(o: { x: number; y: number; h: number }) {
  const s = o.h / D_H;
  return html`
    <g transform="translate(${o.x - D_X * s} ${o.y - D_Y * s}) scale(${s})">${deltaShape()}</g>
  `;
}

/**
 * The delta turned onto a heading, pointing right, its ink box placed at
 * (x, y) with horizontal extent w. Rotating about the ink centre rather than
 * the drawing's origin is what keeps the lean from throwing it off the line.
 */
export function deltaRight(o: { x: number; y: number; w: number }) {
  const s = o.w / D_H;
  const tx = o.x - (D_CX - D_H / 2) * s;
  const ty = o.y - (D_CY - D_W / 2) * s;
  return html`
    <g transform="translate(${tx} ${ty}) scale(${s}) rotate(90 ${D_CX} ${D_CY})">
      ${deltaShape()}
    </g>
  `;
}

/** Height of the heading delta for a given horizontal extent. */
export function deltaRightHeight(w: number) {
  return (w / D_H) * D_W;
}

export type Wordmark = {
  id: string;
  name: string;
  idea: string;
  cost: string;
  /** Intrinsic aspect of the drawing, so a caller can size by height alone. */
  vb: { w: number; h: number };
  art: (face: 'sans' | 'mono') => unknown;
};

export const WORDMARKS: Wordmark[] = [
  {
    id: 'tittle',
    name: 'Tittle',
    idea:
      'The delta becomes the dot of the i. The word is never interrupted, it is set in one face at one weight and reads as a word, and the mark is inside it rather than bolted to the front.',
    cost:
      'The mark is the smallest thing on the lockup, so at a size where the word is still legible the delta is a speck. It also needs a dotless i, which is a real character but not one every face draws well.',
    vb: { w: 104, h: 44 },
    art: (face) => html`
      <!-- A dotless i, written as the literal character rather than as
           &#x131;. The gate strips named and decimal HTML entities before it
           scans prose punctuation but not hex ones, so the entity's own
           semicolon reads as a semicolon in the rendered text. -->
      <!-- The delta sits above it. The word is one
           text run so the face keeps its own kerning, and the delta is placed
           against the render rather than measured, because text width is not
           knowable before the browser has it. -->
      <text
        x="3"
        y="32"
        font-size=${face === 'mono' ? 26 : 30}
        class=${face === 'mono' ? 'wm-mono' : 'wm-sans'}
        fill="currentColor"
      >
        pılots
      </text>
      ${deltaUp({ x: face === 'mono' ? 18.5 : 16.6, y: 3, h: 12 })}
    `,
  },
  {
    id: 'course',
    name: 'Course line',
    idea:
      'A rule running the width of the lockup with the delta riding it on a heading, the word sitting on the same line. The rule is the baseline and the flight path at once, which is the one arrangement here that says aviation without drawing an aeroplane.',
    cost:
      'It only works wide. Squeeze it into a square or a favicon and the rule has nowhere to run, so this treatment needs a separate mark for every cramped placement.',
    vb: { w: 150, h: 44 },
    art: (face) => html`
      ${deltaRight({ x: 0, y: 30 - deltaRightHeight(17), w: 17 })}
      <text
        x="27"
        y="30"
        font-size=${face === 'mono' ? 24 : 27}
        class=${face === 'mono' ? 'wm-mono' : 'wm-sans'}
        fill="currentColor"
      >
        pilots
      </text>
      <!-- Drawn last so it crosses the delta's foot and the word's baseline as
           one line rather than stopping at each of them. -->
      <path d="M0 30.5 H150" stroke="currentColor" stroke-width="1" opacity="0.45" />
    `,
  },
  {
    id: 'plate',
    name: 'Plate',
    idea:
      'An equipment nameplate. Hairline box, the delta in its own compartment, a divider, then the name tracked out in mono. It is the only one that already looks like something screwed to a panel, which is what the rest of the site is drawn to look like.',
    cost:
      'A box around a logo is a container, and containers are the first thing a design system outgrows. It also cannot be placed on anything that already has a border without reading as a nested frame.',
    vb: { w: 152, h: 52 },
    art: (face) => html`
      <rect
        x="0.5"
        y="0.5"
        width="151"
        height="51"
        fill="none"
        stroke="currentColor"
        stroke-width="1"
        opacity="0.55"
      />
      ${deltaUp({ x: 15, y: 14, h: 24 })}
      <path d="M52 0 V52" stroke="currentColor" stroke-width="1" opacity="0.55" />
      <text
        x="68"
        y="33"
        font-size=${face === 'mono' ? 17 : 22}
        letter-spacing=${face === 'mono' ? 1.6 : 1.2}
        class=${face === 'mono' ? 'wm-mono' : 'wm-sans'}
        fill="currentColor"
      >
        pilots
      </text>
    `,
  },
];

/** One whole-word treatment at a given height. */
export function wordmark(w: Wordmark, o: { height: number; face: 'sans' | 'mono' }) {
  return html`
    <svg
      viewBox="0 0 ${w.vb.w} ${w.vb.h}"
      height=${o.height}
      width=${Math.round((o.height * w.vb.w) / w.vb.h)}
      role="img"
      aria-label="pilots"
      class="block"
    >
      ${w.art(o.face)}
    </svg>
  `;
}
