import { html } from '@webjsdev/core';
import { section } from '#lib/ui/section.ts';
import { pageHero } from '#lib/ui/page-hero.ts';
import { PROSE, FIELD_LABEL, PANEL } from '#lib/design/recipes.ts';
import { DELTA, markSvg, type Candidate } from '#lib/design/logo-candidates.ts';

/**
 * /brand
 *
 * The mark, and the three places it has to keep working.
 *
 * This was a bake-off between six drawings. Delta won, so the page is now the
 * shorter thing a settled mark needs: the drawing on both grounds and at
 * favicon sizes, the word beside it, and the header it lives in on nearly every
 * page view. The losing drawings came out of the repo rather than staying
 * behind a flag, because a rejected mark left in the tree gets rendered by
 * accident eventually.
 *
 * It sits in the nav and stays `noindex`. The two audiences want different
 * things: the nav serves people already working on this, the index serves
 * people arriving cold, and a page of design tiles is not a stranger's first
 * search result for a product.
 *
 * The order below is the order a mark actually fails in. Favicon size kills
 * most of them, the inversion kills the next few, and the rest die standing
 * next to the word they have to share a header with.
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
/**
 * The two casings under test.
 *
 * The site writes the name lowercase everywhere, including at the start of a
 * sentence, which is a deliberate choice rather than an oversight. The capital
 * is here because a lockup is where that choice is most visible and least
 * committed: the header can carry one form while the prose carries another, and
 * this is the surface for deciding whether it should.
 */
const WORD_CASES = [
  { id: 'lower', label: 'lowercase', word: 'pilots' },
  { id: 'upper', label: 'capital P', word: 'Pilots' },
];

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
function lockupRow(c: Candidate, word: string, label: string) {
  return html`
    <div class="grid gap-5 items-center py-6 border-b border-rule mid:grid-cols-[9rem_1fr_1fr]">
      <span class=${FIELD_LABEL}>${label}</span>
      ${WORD_FACES.map(
        (w) => html`
          <div class="lab-paper flex items-center gap-2.5">
            ${markSvg(c, 30)}
            <span class="${w.cls} text-[26px] leading-none text-ink">${word}</span>
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
function headerMock(c: Candidate, word: string, label: string) {
  return html`
    <div>
      <p class="${FIELD_LABEL} mb-2">${label}</p>
      <div class="border border-rule bg-paper overflow-hidden">
      <div class="flex items-center gap-3 px-4 h-14">
        <span class="lab-paper flex items-center gap-2 mr-2">
          ${markSvg(c, 22)}
          <span class="font-mono text-[15px] font-semibold tracking-tight text-ink">${word}</span>
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
    </div>
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
    </style>

    ${pageHero({
      heading: 'The mark',
      lede: html`
        Delta, chosen out of six. This page is what it has to keep surviving:
        both grounds, the sizes a favicon renders at, the word beside it, and
        the header it lives in on nearly every page view.
      `,
    })}

    ${section({
      id: 'mark',
      heading: 'The drawing',
      layout: 'split',
      lede: html`
        Shown on deep ink and on warm paper, because a drawing tuned against
        black often goes muddy when it is inverted. The strip underneath is the
        same file at the sizes a favicon and an avatar actually render at, which
        is where most marks fall apart.
      `,
      body: html`
        <div class="grid gap-6 mid:grid-cols-2">
          ${card(DELTA)}
        </div>
      `,
    })}

    ${section({
      id: 'lockups',
      heading: 'Against the word',
      body: html`
        <p class="${PROSE} mb-6">
          Two questions at once, neither settled. Across the columns, whether a
          heavier sans carries the name better than the mono the header uses
          today, which reads as an instrument label and also, at a glance, as a
          filename. Down the rows, whether the name takes a capital. The site
          writes it lowercase everywhere, including at the start of a sentence,
          so a capital here would be a decision rather than a tidy-up.
        </p>
        <div class="border-t border-rule">
          <div class="hidden mid:grid grid-cols-[9rem_1fr_1fr] gap-5 pt-4">
            <span></span>
            ${WORD_FACES.map((w) => html`<span class=${FIELD_LABEL}>${w.label}</span>`)}
          </div>
          ${WORD_CASES.map((k) => lockupRow(DELTA, k.word, k.label))}
        </div>
      `,
    })}

    ${section({
      id: 'in-place',
      heading: 'In the header',
      layout: 'split',
      lede: html`
        The placement that matters most, since it is on every page view. A mark
        that resolves on a card and then vanishes beside six nav links has not
        passed. Both casings are shown at the size they actually render.
      `,
      body: html`
        <div class="grid gap-6 wide:grid-cols-2">
          ${WORD_CASES.map((k) => headerMock(DELTA, k.word, k.label))}
        </div>
      `,
    })}

    <div class="max-w-6xl mx-auto px-6 pb-20">
      <hr class="border-0 border-t border-rule m-0 mb-6" />
      <p class="text-sm text-ink-subtle max-w-[62ch] m-0">
        The mark is settled and the losing drawings are out of the repo. This
        route stays out of the index and the sitemap all the same: it is a
        working surface for whoever is checking the mark still holds up, not a
        brand page for a stranger arriving cold.
      </p>
    </div>
  `;
}
