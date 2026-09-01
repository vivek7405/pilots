import { html } from '@webjsdev/core';
import { section } from '#lib/ui/section.ts';
import { pageHero } from '#lib/ui/page-hero.ts';
import { PROSE, FIELD_LABEL, PANEL } from '#lib/design/recipes.ts';
import { CANDIDATES, markSvg, type Candidate } from '#lib/design/logo-candidates.ts';
import { DELTA_PS, deltaPMark, deltaPLockup, type DeltaP } from '#lib/design/delta-p.ts';

/**
 * /brand
 *
 * The working surface for choosing a mark. Not a brand page: a brand page
 * publishes a decision, and there is no decision yet.
 *
 * It sits in the nav, so anyone working on this can reach it, but it is
 * `noindex` and out of the sitemap. A route showing several competing logos
 * for one product is not what a stranger's first search result should be, and
 * the two audiences want different things: the nav serves people who already
 * know what this is, the index serves people who do not.
 *
 * The layout answers the three questions a mark actually has to survive, in
 * the order they kill candidates:
 *
 *   1. Does it hold together at favicon size? Most marks die here.
 *   2. Does it survive the inversion? A drawing tuned on black often goes
 *      muddy on warm paper, which is why both tiles are shown side by side
 *      rather than one at a time behind a toggle.
 *   3. Does it sit beside the word without fighting it? A mark reviewed
 *      alone is reviewed in a context it will never appear in.
 */
export const metadata = {
  title: 'Brand',
  description: 'Candidate marks for pilots, drawn on one grid and compared at working sizes.',
  /**
   * Linked from the nav but kept out of the index, which is not a
   * contradiction. The nav is for people who are already here and want to see
   * where the mark stands. The index is for people arriving cold, and a
   * stranger's first result for this product should not be a page of logos
   * that disagree with each other. When one is chosen this becomes a real
   * brand page and the flag comes off.
   */
  robots: { index: false, follow: false },
};

/** The sizes a mark has to survive, smallest first, because that is the order they fail in. */
const SMALL_SIZES = [32, 24, 20, 16];

/**
 * The two wordmark voices under test.
 *
 * The header ships the mono one today. The sans one is here because a mono
 * wordmark is a strong instrument-panel signal and a weak brand signal: it
 * looks like a filename, which is fine in a header and thin on a title slide.
 */
const WORD_FACES = [
  { id: 'mono', label: 'mono, semibold', cls: 'font-mono font-semibold tracking-tight' },
  { id: 'sans', label: 'sans, extrabold', cls: 'font-sans font-extrabold tracking-[-0.035em]' },
];

/** One tile, one background, one mark. The class carries both the ink and the paper. */
function tile(c: Candidate, tone: 'dark' | 'light') {
  return html`
    <div class="lab-${tone} flex-1 aspect-4/3 grid place-items-center">${markSvg(c, 60)}</div>
  `;
}

/**
 * A candidate card.
 *
 * Heading first, then the artwork, then the small-size strip, then the two
 * sentences. The label under the strip labels the VALUES beside it rather
 * than introducing a heading, which is the distinction AGENTS.md invariant 12
 * turns on.
 */
function card(c: Candidate) {
  return html`
    <article class="${PANEL} overflow-hidden flex flex-col">
      <div class="flex border-b border-rule">${tile(c, 'dark')}${tile(c, 'light')}</div>

      <div class="p-5 flex flex-col gap-4 flex-1">
        <div class="flex items-baseline justify-between gap-3">
          <h3 class="text-h3 font-bold m-0">${c.name}</h3>
          <code class="font-mono text-xs text-ink-subtle">${c.id}</code>
        </div>

        <div class="lab-paper flex items-end gap-4 border-y border-rule py-3">
          ${SMALL_SIZES.map(
            (px) => html`
              <span class="flex flex-col items-center gap-1.5">
                ${markSvg(c, px)}
                <span class="font-mono text-[10px] text-ink-subtle leading-none">${px}px</span>
              </span>
            `,
          )}
        </div>

        <p class="${PROSE} text-sm m-0">${c.idea}</p>
        <p class="text-sm text-ink-subtle leading-[1.65] m-0 mt-auto pt-1">
          <span class="text-ink font-semibold">The cost.</span> ${c.cost}
        </p>
      </div>
    </article>
  `;
}

/**
 * A lockup row: the mark against the word, in both wordmark voices.
 *
 * The mark is set at cap height rather than at line height. Matching the
 * mark's box to the text's box is the usual mistake and it always leaves the
 * mark looking a size too big, because a lowercase word's visual mass sits
 * well inside its em.
 */
function lockupRow(c: Candidate) {
  return html`
    <div class="grid gap-5 items-center py-6 border-b border-rule mid:grid-cols-[9rem_1fr_1fr]">
      <code class="font-mono text-xs text-ink-subtle">${c.id}</code>
      ${WORD_FACES.map(
        (w) => html`
          <div class="lab-paper flex items-center gap-2.5">
            ${markSvg(c, 30)}
            <span class="${w.cls} text-[26px] leading-none text-ink">pilots</span>
          </div>
        `,
      )}
    </div>
  `;
}

/**
 * The header strip, reproduced close enough to judge against.
 *
 * A mark that looks resolved on a white card and then disappears next to
 * four nav links has not passed anything, and the header is the placement
 * this mark will actually live in on nearly every page view.
 */
function headerMock(c: Candidate) {
  return html`
    <div class="border border-rule bg-paper overflow-hidden">
      <div class="flex items-center gap-3 px-4 h-14">
        <span class="lab-paper flex items-center gap-2 mr-2">
          ${markSvg(c, 22)}
          <span class="font-mono text-[15px] font-semibold tracking-tight text-ink">pilots</span>
        </span>
        <span class="hidden mid:flex items-center gap-4 text-sm text-ink-muted">
          <span>Sandboxes</span><span>Deploy</span><span>Architecture</span>
        </span>
        <span
          class="ml-auto h-8 px-4 grid place-items-center rounded-full bg-signal text-signal-ink text-[13px] font-semibold"
          >GitHub</span
        >
      </div>
    </div>
  `;
}

/**
 * One construction, shown as the mark alone and as the whole name.
 *
 * Both come off the same drawing, so a variant that only works in one of the
 * two has failed. The mark is not forced into a square: a quarter-turned delta
 * is wider than it is tall, and the sibling brand's own monogram file is a
 * wide crop for the same reason.
 */
function deltaPCard(v: DeltaP) {
  return html`
    <article class="${PANEL} overflow-hidden">
      <div class="lab-dark px-8 py-9 flex items-center justify-center">
        ${deltaPLockup(v, { height: 58 })}
      </div>
      <div class="flex items-baseline justify-between gap-3 px-5 pt-4 border-t border-rule">
        <h3 class="text-h3 font-bold m-0">${v.name}</h3>
        <code class="font-mono text-xs text-ink-subtle">${v.id}</code>
      </div>
      <div class="lab-paper px-5 divide-y divide-rule">
        <div class="py-5 flex flex-wrap items-center gap-x-8 gap-y-4">
          ${deltaPMark(v, 60)}
          <span class="flex items-end gap-4">
            ${SMALL_SIZES.map(
              (px) => html`
                <span class="flex flex-col items-center gap-1.5">
                  ${deltaPMark(v, px)}
                  <span class="font-mono text-[10px] text-ink-subtle leading-none">${px}px</span>
                </span>
              `,
            )}
          </span>
        </div>
        <div class="py-5 flex flex-wrap items-center gap-x-6 gap-y-3">
          ${deltaPLockup(v, { height: 38 })}
          <span class=${FIELD_LABEL}>sans</span>
        </div>
        <div class="py-5 flex flex-wrap items-center gap-x-6 gap-y-3">
          ${deltaPLockup(v, { height: 20 })}
          <span class=${FIELD_LABEL}>at header size</span>
        </div>
      </div>
      <div class="px-5 py-5 border-t border-rule flex flex-col gap-3">
        <p class="${PROSE} text-sm m-0">${v.idea}</p>
        <p class="text-sm text-ink-subtle leading-[1.65] m-0">
          <span class="text-ink font-semibold">The cost.</span> ${v.cost}
        </p>
      </div>
    </article>
  `;
}

export default function LogoLabPage() {
  return html`
    <style>
      /* The two review tiles are fixed colours on purpose. They are not the
         page theme, they are the two backgrounds a logo file has to be
         correct on, so they must not follow the reader's toggle. The values
         are the palette's own deep ink and elevated paper.

         Everything else resolves through the live tokens, so a mark shown
         "on paper" really is on this page's paper in whichever theme the
         reader is in. No colour is declared twice here, which is what
         AGENTS.md invariant 11 is about. */
      .lab-dark {
        --logo-bg: #0e1014;
        background: #0e1014;
        color: #e9e7e1;
      }
      .lab-light {
        --logo-bg: #fffdf9;
        background: #fffdf9;
        color: #16181c;
      }
      .lab-paper {
        --logo-bg: var(--paper-elev);
        color: var(--ink);
      }
      /* Sets the one custom property every mark reads for its accented part. */
      .lab-accent {
        --logo-accent: var(--signal);
      }
      /* The wordmark faces. Set on the SVG text rather than passed as a
         font-family attribute, because the attribute does not resolve a
         custom property and the whole point is to use the site's own stack. */
      .lockup-sans {
        font-family: var(--font-sans);
        font-weight: 900;
        letter-spacing: -0.04em;
      }
      .lockup-mono {
        font-family: var(--font-mono);
        font-weight: 600;
        letter-spacing: -0.02em;
      }
      /* The whole-word treatments. Heavier than the lockup faces above,
         because here the type carries the mark rather than sitting beside a
         drawn letter that was doing the carrying. */
      .dp-sans {
        font-family: var(--font-sans);
        font-weight: 800;
        letter-spacing: -0.035em;
      }
      .dp-mono {
        font-family: var(--font-mono);
        font-weight: 600;
      }
    </style>

    ${pageHero({
      heading: 'Logo lab',
      lede: html`
        Six candidate marks, drawn on one grid so they can be compared instead of
        admired one at a time. Nothing here is chosen. Each card states the idea
        the drawing carries and what that idea costs.
      `,
    })}

    ${section({
      id: 'candidates',
      heading: 'The candidates',
      layout: 'split',
      lede: html`
        Each is shown on deep ink and on warm paper, because a drawing tuned
        against black often goes muddy when it is inverted. The strip underneath
        is the same file at the sizes a favicon and an avatar actually render at,
        which is where most marks fall apart.
      `,
      body: html`
        <div class="grid gap-6 mid:grid-cols-2 wide:grid-cols-3">
          ${CANDIDATES.map((c) => card(c))}
        </div>
      `,
    })}

    ${section({
      id: 'delta-p',
      heading: 'The delta over the word',
      layout: 'split',
      lede: html`
        The delta exactly as drawn, turned and set above the word so it sits
        over the i and the l rather than across them, with a thin stem dropped
        from the cut the mark already carries. Nothing here reshapes it.
      `,
      body: html`
        <p class="${PROSE} mb-9">
          A quarter turn on its own cannot clear both letters. The wing's
          underside runs at about fifteen degrees there, while the step from the
          i's shoulder up to the l's ascender is nearer forty. Lift it clear of
          the l and it floats far above the i. Turning the mark back to
          sixty-four degrees puts that underside on the slope the word actually
          makes, so the wing follows the letters instead of ignoring them.
          Which edge is even the underside changes with the turn, so the drawing
          is measured rather than reasoned about: the lean means the mark's own
          axis is already some twenty degrees off vertical, and working from the
          upright shape gets the angle wrong by that much.
        </p>
        <p class="${PROSE} mb-9">
          The stem is the mark's own partition, carried through the same
          transform and run down to the baseline, drawn at the weight of the
          letters around it. The word sits at ordinary spacing in every variant
          and the delta is what moves, which is what keeps a gap from opening up
          beside the i. The i is dotless, since the wing is over it.
        </p>
        <div class="grid gap-6 mid:grid-cols-2">
          ${DELTA_PS.map((v) => deltaPCard(v))}
        </div>
      `,
    })}

    ${section({
      id: 'lockups',
      heading: 'Against the word',
      body: html`
        <p class="${PROSE} mb-6">
          The header sets the name in mono today, which reads as an instrument
          label and also, at a glance, as a filename. The second column asks
          whether a heavier sans carries the name better once the mark is doing
          the technical talking.
        </p>
        <div class="border-t border-rule">
          <div class="hidden mid:grid grid-cols-[9rem_1fr_1fr] gap-5 pt-4">
            <span></span>
            ${WORD_FACES.map((w) => html`<span class=${FIELD_LABEL}>${w.label}</span>`)}
          </div>
          ${CANDIDATES.map((c) => lockupRow(c))}
        </div>
      `,
    })}

    ${section({
      id: 'in-place',
      heading: 'In the header',
      layout: 'split',
      lede: html`
        The placement that matters most, since it is on every page view. A mark
        that resolves on a card and then vanishes beside four nav links has not
        passed.
      `,
      body: html`
        <div class="grid gap-4 wide:grid-cols-2">
          ${CANDIDATES.map((c) => headerMock(c))}
        </div>
      `,
    })}

    <div class="max-w-6xl mx-auto px-6 pb-20">
      <hr class="border-0 border-t border-rule m-0 mb-6" />
      <p class="text-sm text-ink-subtle max-w-[62ch] m-0">
        This route is in the nav but stays out of the index and the sitemap
        while the marks still disagree with each other. When one wins it moves
        into a brand module beside the other design tokens, the losers come out
        of the repo, and what is left here becomes a real brand page.
      </p>
    </div>
  `;
}
