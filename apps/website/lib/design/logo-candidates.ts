import { html } from '@webjsdev/core';

/**
 * The logo candidates.
 *
 * A trial set, not a decision. It lives in `lib/design/` rather than beside
 * the page that shows it because exactly one of these eventually becomes
 * `brandMark()`, and a mark that has to be retyped when it wins arrives at
 * the header subtly different from the one that was reviewed.
 *
 * THREE RULES THE WHOLE SET OBEYS, so the comparison is about the ideas
 * rather than about who drew a bigger circle:
 *
 * 1. Every mark is authored on the SAME 32-unit grid, which is the favicon
 *    grid. Designing at hero size and shrinking is how a mark ends up with a
 *    detail that only exists above 64 pixels. These were drawn small first.
 * 2. Ink is `currentColor` and paper is `var(--logo-bg)`. That is what lets
 *    one drawing sit on a dark tile, a light tile, and the live page without
 *    a second file, and it is why the negative space really is negative
 *    space rather than a hard-coded near-black that goes wrong on paper.
 * 3. One part of each mark reads `var(--logo-accent, …)`, so a container
 *    that sets `--logo-accent` gets the acid-green variant and every other
 *    container gets the monochrome one. No branching, no second drawing.
 *
 * The sibling site's marks are greyscale throughout, and AGENTS.md invariant
 * 2 rations the accent hard. The monochrome column is therefore the default
 * and the accent column is the thing being questioned.
 */
export type Candidate = {
  id: string;
  name: string;
  /** The single idea the drawing carries. */
  idea: string;
  /** What it costs. Every one of these trades something away. */
  cost: string;
  /** The artwork, on a 32-unit grid. A function, so each render is its own nodes. */
  art: () => unknown;
};

/** Ink that turns acid green when the container sets `--logo-accent`. */
const ACCENT_INK = 'var(--logo-accent, currentColor)';
/** Negative space that turns acid green when the container sets `--logo-accent`. */
const ACCENT_VOID = 'var(--logo-accent, var(--logo-bg))';

/**
 * The delta's geometry, before the lean, in its own 32-unit grid.
 *
 * Named because two candidates now share the drawing and because the cut is
 * derived from these points rather than typed as coordinates.
 */
const D_APEX = { x: 16, y: 3.8 };
const D_TIP_L = { x: 5.2, y: 27.6 };
const D_TIP_R = { x: 26.8, y: 27.6 };
const D_NOTCH = { x: 16, y: 21.4 };

/**
 * The cut.
 *
 * It sits BELOW the notch apex, and that is the whole point of where it is.
 * The sibling brand's W carries the same device and does not show the same
 * problem, and measuring the two explained why: its cut breaks into three
 * segments because the letter has gaps between its strokes at that height,
 * the longest run being under a quarter of the mark's width. Ours ran clean
 * through a solid triangle as ONE channel spanning well over half the mark.
 *
 * A single long parallel-sided channel is a shape whose width the eye reads
 * directly, so irradiation (light areas appearing to swell into dark ones)
 * makes it look wider on paper than on ink even though it measures the same.
 * Broken into segments there is no continuous width left to compare, which is
 * why the W looks even in both themes.
 *
 * The delta has exactly one hole to break the cut on, its tail notch, so the
 * cut is placed low enough to cross it. That splits the mark into an upper
 * body and two separated wingtips, which is the same three-piece result the W
 * gets for free from its own strokes.
 */
const CUT_TOP = 22.6;
const CUT_BOTTOM = 24.4;

/** x on the line from `a` to `b` at height y. */
function xAt(a: { x: number; y: number }, b: { x: number; y: number }, y: number) {
  return a.x + ((y - a.y) / (b.y - a.y)) * (b.x - a.x);
}

/**
 * The delta as SEPARATE PATHS with real air between them.
 *
 * This is the sibling brand's construction rather than a painted band: its W
 * ships as two path elements with a gap in the geometry, so one file is
 * correct on ink, on paper, and on a photograph. A bar filled in the
 * background colour only works where the container has declared that colour,
 * which every tile in this lab does and nothing outside it need.
 */
function deltaArt(fill: string) {
  const oL = (y: number) => xAt(D_APEX, D_TIP_L, y);
  const oR = (y: number) => xAt(D_APEX, D_TIP_R, y);
  const nL = (y: number) => xAt(D_NOTCH, D_TIP_L, y);
  const nR = (y: number) => xAt(D_NOTCH, D_TIP_R, y);
  const f = (n: number) => n.toFixed(2);
  const body =
    `M${D_APEX.x} ${D_APEX.y} L${f(oR(CUT_TOP))} ${CUT_TOP} L${f(nR(CUT_TOP))} ${CUT_TOP} ` +
    `L${D_NOTCH.x} ${D_NOTCH.y} L${f(nL(CUT_TOP))} ${CUT_TOP} L${f(oL(CUT_TOP))} ${CUT_TOP} Z`;
  const wingL =
    `M${f(oL(CUT_BOTTOM))} ${CUT_BOTTOM} L${f(nL(CUT_BOTTOM))} ${CUT_BOTTOM} L${D_TIP_L.x} ${D_TIP_L.y} Z`;
  const wingR =
    `M${f(nR(CUT_BOTTOM))} ${CUT_BOTTOM} L${f(oR(CUT_BOTTOM))} ${CUT_BOTTOM} L${D_TIP_R.x} ${D_TIP_R.y} Z`;
  return html`
    <g transform="translate(5.4 0)">
      <g transform="skewX(-11)">
        <path d=${body} fill=${fill} />
        <path d=${wingL} fill=${fill} />
        <path d=${wingR} fill=${fill} />
      </g>
    </g>
  `;
}

export const CANDIDATES: Candidate[] = [
  {
    id: 'horizon',
    name: 'Horizon',
    idea:
      'The attitude indicator, the instrument that tells a pilot which way is up. Ground below the horizon, a climbing datum above it, and one slice cutting the whole disc as a single object.',
    cost:
      'The filled ground makes it the heaviest mark here on a light background, and the wing datum is the first detail to thicken into a smudge at favicon size.',
    art: () => html`
      <!-- Ground: a circular segment drawn explicitly rather than clipped, so
           the mark carries no clip-path id at all. Ids collide the moment a
           page renders the same mark twice, and this page renders each one
           nine times over. The chord half-width is the square root of
           11.4 squared minus 1.2 squared. -->
      <path d="M4.66 17.2 L27.34 17.2 A11.4 11.4 0 0 1 4.66 17.2 Z" fill=${ACCENT_INK} />
      <!-- The aircraft datum: the fixed wing symbol an attitude indicator
           carries, wings out and a notch at the nose. The first draft used a
           plain triangle sitting on the horizon and the mark read as a
           sunrise, which is a stock icon and not this product. -->
      <path
        d="M5.4 15.4 H12.6 L16 18.9 L19.4 15.4 H26.6"
        fill="none"
        stroke="currentColor"
        stroke-width="2.5"
        stroke-linejoin="miter"
        stroke-linecap="butt"
      />
      <circle cx="16" cy="16" r="12.6" fill="none" stroke="currentColor" stroke-width="2.4" />
      <!-- The slice runs past the bezel on both sides, so it cuts the ring as
           well as the ground. One cut through one object, which is the idiom
           the sibling brand already uses.

           It sits low, near the base of the disc, and that is not a taste
           call. Level with the datum it became a third horizontal stroke and
           the three of them read as a face with its eyes shut. -->
      <rect x="2.6" y="23.2" width="26.8" height="1.9" fill="var(--logo-bg)" />
    `,
  },
  {
    id: 'monogram',
    name: 'Sliced p',
    idea:
      'The initial built as geometry rather than set in a typeface. Stem, bowl, and descender, cut by one band of negative space that runs straight through both.',
    cost:
      'It is a letter, so it inherits the letter problem. A geometric lowercase p is a shape several other products already have some claim on.',
    art: () => html`
      <path
        d="M9.2 8.4 V26"
        fill="none"
        stroke="currentColor"
        stroke-width="4.6"
        stroke-linecap="butt"
      />
      <path
        d="M9.2 8.4 H17.4 a5.6 5.6 0 0 1 0 11.2 H9.2"
        fill="none"
        stroke=${ACCENT_INK}
        stroke-width="4.6"
        stroke-linecap="butt"
        stroke-linejoin="miter"
      />
      <rect x="4" y="12.9" width="23" height="2.2" fill="var(--logo-bg)" />
    `,
  },
  {
    id: 'delta',
    name: 'Delta',
    idea:
      'The aircraft symbol off a moving map, swept back and leaning forward. The only mark in the set that carries motion in the drawing itself. Its cut is real air rather than a painted bar, and it crosses the tail notch, so the mark ships as a body and two separated wingtips.',
    cost:
      'An upward triangle sits close to a play button, and the lean is the only thing holding the two apart. Placing the cut low enough to be broken also puts it low on the mark, and the wingtips it leaves behind are small enough to close up first at favicon size.',
    art: () => deltaArt(ACCENT_INK),
  },
  {
    id: 'delta-90',
    name: 'Delta 90',
    idea:
      'The same drawing given a quarter turn, nose on a heading rather than climbing. Nothing else changes: the same path, the same lean, the same partition, which now stands up as a vertical cut instead of lying across the mark.',
    cost:
      'Turned this way it reads as a cursor or a play control before it reads as an aircraft, which are two of the most heavily spoken-for shapes in software. Pointing up, the climb was what kept it out of their company.',
    art: () => html`
      <!-- The same three paths, turned. Rotating about the tile's centre rather
           than the ink's costs nothing here, the two being within a third of a
           unit of each other, and it keeps this readable beside the upright
           version above. -->
      <g transform="rotate(90 16 16)">${deltaArt(ACCENT_INK)}</g>
    `,
  },
  {
    id: 'two-faces',
    name: 'Two faces',
    idea:
      'One primitive wearing two faces. A single square split down the middle, solid on one side and hollow on the other, so the silhouette stays one shape while the two halves plainly are not the same thing.',
    cost:
      'It is the most abstract mark here. Nothing in the drawing says aviation, computing, or pilots, so it leans entirely on the wordmark beside it.',
    art: () => html`
      <!-- Drawn as two explicit half-shapes rather than a square with a bar
           across it. The first attempt was a filled square with a second one
           outlined and offset over it, which is the copy glyph almost exactly,
           and no amount of tuning the offset moves it off that reading. -->
      <path
        d="M14.6 4 H9 a5 5 0 0 0 -5 5 V23 a5 5 0 0 0 5 5 H14.6 Z"
        fill="currentColor"
      />
      <path
        d="M17.6 5.3 H23 a3.7 3.7 0 0 1 3.7 3.7 V22.7 a3.7 3.7 0 0 1 -3.7 3.7 H17.6 Z"
        fill="none"
        stroke=${ACCENT_INK}
        stroke-width="2.6"
        stroke-linejoin="round"
      />
    `,
  },
  {
        id: 'no-centre',
    name: 'No centre',
    idea:
      'Four identical peers with nothing in the middle of them. Every host runs the same stack and none of them is the one that matters, which is the hardest thing about pilots to say in words.',
    cost:
      'The weakest of the set, and the reason is worth stating. Every literal drawing of this idea lands on a glyph that already means something else, so it is fighting for a reading before it starts.',
    art: () => html`
      <!-- Third attempt at this one, and the failures are the useful part.
           Four nodes joined by hairlines fused into a d-pad. A dashed ring
           with four gaps came out as a lifebuoy. Bare nodes on the diagonals
           are the least encumbered version: rotating the set off the 2x2 grid
           is what keeps it from being the application-launcher glyph. -->
      <rect x="12.2" y="2.6" width="7.6" height="7.6" rx="2" fill="currentColor" />
      <rect x="21.8" y="12.2" width="7.6" height="7.6" rx="2" fill=${ACCENT_INK} />
      <rect x="12.2" y="21.8" width="7.6" height="7.6" rx="2" fill="currentColor" />
      <rect x="2.6" y="12.2" width="7.6" height="7.6" rx="2" fill="currentColor" />
    `,
  },
  {
    id: 'runway',
    name: 'Runway',
    idea:
      'A runway in perspective with its centreline punched out. The negative space is the marking, so the cut does real work here rather than being applied on top.',
    cost:
      'It says aviation louder than it says compute, and the perspective wants room, so it is the first of these to fail in a cramped placement.',
    art: () => html`
      <path d="M11.4 28.4 L20.6 28.4 L18.4 5 L13.6 5 Z" fill="currentColor" />
      <rect x="15" y="24.6" width="2" height="2.8" fill=${ACCENT_VOID} />
      <rect x="15.13" y="19.4" width="1.75" height="2.4" fill=${ACCENT_VOID} />
      <rect x="15.25" y="14.8" width="1.5" height="2" fill=${ACCENT_VOID} />
      <rect x="15.35" y="10.8" width="1.3" height="1.6" fill=${ACCENT_VOID} />
    `,
  },
];

/**
 * One candidate at one size.
 *
 * `aria-hidden`, always. The page renders every mark nine times over, and a
 * labelled image repeated nine times is nine announcements of the same word.
 * The card heading names the candidate once, which is the accessible version
 * of the same information.
 */
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
