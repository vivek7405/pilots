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
      'The aircraft symbol off a moving map, swept back and leaning forward. The only mark in the set that carries motion in the drawing itself.',
    cost:
      'An upward triangle sits close to a play button, and the lean is the only thing holding the two apart.',
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
  },
  {
    id: 'delta-90',
    name: 'Delta 90',
    idea:
      'The same drawing given a quarter turn, nose on a heading rather than climbing. Nothing else changes: the same path, the same lean, the same partition, which now stands up as a vertical cut instead of lying across the mark.',
    cost:
      'Turned this way it reads as a cursor or a play control before it reads as an aircraft, which are two of the most heavily spoken-for shapes in software. Pointing up, the climb was what kept it out of their company.',
    art: () => html`
      <!-- The quarter turn is applied to the whole mark, partition included, and
           about the tile's centre rather than the ink's. The two are within a third of
           a unit of each other, so turning about the tile costs nothing and
           keeps this readable beside the upright version above.

           The slice is emitted after the path, inside the same rotation, so it
           still cuts what it cut before. -->
      <g transform="rotate(90 16 16)">
        <g transform="translate(5.4 0)">
          <g transform="skewX(-11)">
            <path d="M16 3.8 L26.8 27.6 L16 21.4 L5.2 27.6 Z" fill=${ACCENT_INK} />
          </g>
        </g>
        <rect x="3" y="17.6" width="26" height="2" fill="var(--logo-bg)" />
      </g>
    `,
  },
  {
    id: 'delta-half',
    name: 'Delta half',
    idea:
      'The mark opened along its own axis, so the flyer is two mirror halves rather than one body. The tail notch already began that split and the cut only carries it through to the tip. The turn is set so the gap lies flat rather than following the tile.',
    cost:
      'Nothing joins the halves, so the mark depends entirely on the reader closing the gap themselves. It is also the one drawing here with no solid mass at all, which is what a favicon has the least of to work with.',
    art: () => html`
      <!-- The cut sits INSIDE the skew, unlike the partition on the two cards
           above, and that is the whole trick. The delta is symmetric about
           x = 16 before it is leaned (its apex and its notch both sit on that
           line), so a vertical band there halves it exactly. Being inside the
           skew, the band leans with the drawing and therefore follows the
           mark's own axis rather than the tile's, which is what makes it run
           tip to tail instead of merely crossing the shape.

           The turn is seventy-nine degrees, not ninety, and that number is
           forced rather than chosen. The skew has already tilted the band
           eleven degrees off vertical, so laying it over with a full quarter
           turn would leave it eleven degrees off horizontal. Turning by
           ninety minus eleven lands it flat: the band's direction after the
           skew is (-tan 11, 1), and rotating that by seventy-nine gives a
           vertical component of -tan 11 times sin 79 plus cos 79, which is
           zero to six decimal places.

           Turning by less than a quarter also throws the mark off centre in
           the tile, so the translate puts the ink back. Its two numbers were
           measured off the render rather than derived, because the cut runs
           through the apex as well as the body: the tip is split, both halves
           end short of where the original point was, and the bounding box is
           not the one the four path vertices predict.

           Delta and Delta 90 need no such correction, a quarter turn happening
           to leave them centred.

           The band runs into the tail notch, which is already a void on the
           same line. The two join, and the halves come apart cleanly rather
           than staying pinned together at the back.

           There is no second partition here. Keeping the one from Delta 90 as
           well would cross this at right angles and leave four pieces, and the
           brief was two halves. -->
      <g transform="translate(1.35 -2.22) rotate(79 16 16)">
        <g transform="translate(5.4 0)">
          <g transform="skewX(-11)">
            <path d="M16 3.8 L26.8 27.6 L16 21.4 L5.2 27.6 Z" fill=${ACCENT_INK} />
            <rect x="15" y="2" width="2" height="28" fill="var(--logo-bg)" />
          </g>
        </g>
      </g>
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
