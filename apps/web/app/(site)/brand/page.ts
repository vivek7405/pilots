import { html, asset } from '@webjsdev/core';
import { section } from '#site/lib/ui/section.ts';
import { pageHero } from '#site/lib/ui/page-hero.ts';
import { PROSE, FIELD_LABEL, PANEL, BTN_PRIMARY, BTN_GHOST, LINK } from '#site/lib/design/recipes.ts';
import { DELTA, markSvg, type Candidate } from '#site/lib/design/logo-candidates.ts';
import { PALETTE } from '#site/lib/design/palette.ts';
import { GH_URL, NEW_TAB } from '#site/lib/links.ts';

/**
 * /brand
 *
 * The brand guidelines and the downloadable mark, for anyone who has to show
 * Pilots somewhere that is not this site: an article, a talk, an integration
 * page, a badge.
 *
 * Three things this page must keep doing, none of which a test can see:
 *
 * 1. The mark is RENDERED from `logo-candidates.ts`, the same function the
 *    header and the footer call, so the guidelines cannot show a drawing the
 *    site does not ship.
 * 2. The swatches PAINT the live tokens. The hex strings beside them come from
 *    `palette.ts`, which a test holds against the stylesheet.
 * 3. The clear-space and minimum-size rules are drawn. A rule stated only in a
 *    sentence is a rule nobody follows.
 *
 * The sections run in the order a mark fails in. Small sizes break most
 * drawings, inversion breaks the next few, and the rest go wrong beside the
 * word they have to share a header with.
 */
export const metadata = {
  title: 'Brand',
  description:
    'The Pilots mark as downloadable SVG files, with the rules that keep it legible, the palette, the type, and how the name is written.',
};

/** The sizes the mark is checked at, largest first, down to the floor. */
const SMALL_SIZES = [32, 24, 20, 16];

/** The files, in the order somebody is likely to need them. */
const FILES = [
  {
    file: 'pilots-mark-ink.svg',
    name: 'Mark, ink',
    use: 'The default. Dark ink with no background of its own, for paper, white, and any light surface.',
  },
  {
    file: 'pilots-mark-paper.svg',
    name: 'Mark, paper',
    use: 'The same drawing in light ink, for dark surfaces, slides and terminals.',
  },
];

/**
 * How the name is written, and the forms that turn up in its place.
 *
 * The capital in prose is a readability rule before it is a style one. The
 * name is an ordinary English plural, so a lowercase one makes a sentence like
 * "pilots restores a snapshot" read as a grammar mistake until the reader
 * works out it is a name.
 */
const NAME_FORMS = [
  { form: 'Pilots', ok: true, note: 'In a sentence, a heading or a title. It is a product name and takes a capital like any other.' },
  { form: 'pilots', ok: true, note: 'Only as the wordmark beside the mark, and wherever it is typed, such as a package name or an address.' },
  { form: 'pilot', ok: true, note: 'The command line tool, and only that. The platform is plural.' },
  { form: 'PILOTS', ok: false, note: 'It is a word and it is never set as an acronym.' },
  { form: 'Pilot', ok: false, note: 'The singular is the command, which is always lowercase.' },
];

/** One tile, one background, one mark. The class carries both the ink and the paper. */
function tile(c: Candidate, tone: 'dark' | 'light') {
  return html`
    <div class="lab-${tone} flex-1 aspect-4/3 grid place-items-center">${markSvg(c, 60)}</div>
  `;
}

/**
 * The mark's card: both grounds, then the small-size strip, then what it is
 * and what may not change about it. The label under the strip labels the
 * VALUES beside it, which is the distinction AGENTS.md invariant 12 turns on.
 */
function card(c: Candidate) {
  return html`
    <article class="${PANEL} overflow-hidden flex flex-col">
      <div class="flex border-b border-rule">${tile(c, 'dark')}${tile(c, 'light')}</div>

      <div class="p-5 flex flex-col gap-4 flex-1">
        <h3 class="text-h3 font-bold m-0">${c.name}</h3>

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
          <span class="text-ink font-semibold">What may not change.</span> ${c.cost}
        </p>
      </div>
    </article>
  `;
}

/**
 * The clear-space figure.
 *
 * Drawn on a 48-unit canvas: the mark's own 32-unit box in the middle and a
 * quarter of that box, 8 units, kept empty on every side. The header follows
 * the same rule, since the word sits a little further than that from the mark.
 */
function clearSpace(c: Candidate) {
  return html`
    <div class="${PANEL} lab-paper p-6 grid place-items-center">
      <svg viewBox="0 0 48 48" class="block w-full max-w-[15rem] h-auto" role="img" aria-label="The mark with clear space around it">
        <rect x="0.5" y="0.5" width="47" height="47" fill="none" stroke="var(--rule-strong)" stroke-width="0.5" stroke-dasharray="1.5 1.5" />
        <rect x="8" y="8" width="32" height="32" fill="none" stroke="var(--rule)" stroke-width="0.5" />
        <g transform="translate(8 8)">${c.art()}</g>
      </svg>
    </div>
  `;
}

/**
 * The header strip, reproduced from the real one.
 *
 * The placement that matters most, since it is on every page view. A mark
 * that looks resolved on a card and then disappears next to the nav links has
 * not passed anything.
 */
function headerMock(c: Candidate) {
  return html`
    <div class="border border-rule bg-paper overflow-hidden">
      <div class="flex items-center gap-3 px-4 h-14">
        <span class="on-paper text-ink flex items-center gap-2 mr-2">
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

export default function BrandPage() {
  return html`
    <style>
      /* The two review tiles are fixed colours on purpose. They are not the
         page theme, they are the two backgrounds a logo file has to be
         correct on, so they must not follow the reader's toggle. The values
         are the palette's own deep ink and elevated paper.

         Everything else resolves through the live tokens, so a mark shown
         "on paper" really is on this page's paper in whichever theme the
         reader is in. */
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
      /* One class per swatch in palette.ts, painted from the token itself. */
      .sw-paper {
        background: var(--paper);
      }
      .sw-paper-elev {
        background: var(--paper-elev);
      }
      .sw-ink {
        background: var(--ink);
      }
      .sw-ink-muted {
        background: var(--ink-muted);
      }
      .sw-ink-subtle {
        background: var(--ink-subtle);
      }
      .sw-rule {
        background: var(--rule);
      }
      .sw-signal {
        background: var(--signal);
      }
      .sw-alert {
        background: var(--alert);
      }
    </style>

    ${pageHero({
      heading: 'The mark, the colours and the name',
      lede: html`
        Everything needed to show Pilots somewhere other than this site. The
        mark as files, the rules that keep it legible, the palette, the type,
        and how the name is written.
      `,
      actions: html`
        <a class=${BTN_PRIMARY} href=${asset('/public/brand/pilots-mark-ink.svg')} download>Download the mark</a>
        <a class=${BTN_GHOST} href="#usage">Usage</a>
      `,
    })}

    ${section({
      id: 'mark',
      heading: 'The mark',
      layout: 'split',
      lede: html`
        Shown on deep ink and on warm paper, because a drawing tuned against
        black often goes muddy when it is inverted. The strip underneath is the
        same drawing at the sizes a favicon and an avatar render at, which is
        where most marks fall apart.
      `,
      body: html`
        <div class="grid gap-8 wide:grid-cols-[1fr_1fr] wide:items-start">
          ${card(DELTA)}

          <ul class="m-0 p-0 list-none border-t border-rule">
            ${FILES.map(
              (f) => html`
                <li class="py-5 border-b border-rule flex flex-col gap-2">
                  <div class="flex items-baseline justify-between gap-4">
                    <span class="font-semibold">${f.name}</span>
                    <a class="${LINK} font-mono text-xs" href=${asset('/public/brand/' + f.file)} download>${f.file}</a>
                  </div>
                  <p class="text-sm text-ink-muted m-0 max-w-[52ch]">${f.use}</p>
                </li>
              `,
            )}
            <li class="py-5 text-sm text-ink-subtle max-w-[52ch]">
              Both are vector files with the band cut out for real, so they need no background and
              scale to any size. There is no file for the word, because the word is set in type.
            </li>
          </ul>
        </div>
      `,
    })}

    ${section({
      id: 'space',
      heading: 'Clear space and the smallest size',
      body: html`
        <div class="grid gap-8 mid:grid-cols-[minmax(0,18rem)_1fr] mid:items-center">
          ${clearSpace(DELTA)}
          <div class="flex flex-col gap-6">
            <div>
              <p class="font-semibold m-0 mb-1.5">Keep a quarter of the mark clear on every side</p>
              <p class="${PROSE} text-sm m-0">
                The dashed line is the edge nothing else may cross, and the inner box is the mark's
                own. Text, other logos and the edge of a card all stay outside the dashed line. The
                header on this site follows the same rule.
              </p>
            </div>
            <div>
              <p class="font-semibold m-0 mb-1.5">Sixteen pixels is the floor</p>
              <p class="${PROSE} text-sm m-0">
                At that size the band is a single pixel tall. Any smaller and it closes up, and a
                leaning triangle without its cut reads as a play button. Where the space is
                smaller than that, write the name instead.
              </p>
            </div>
            <div>
              <p class="font-semibold m-0 mb-1.5">Leave the drawing alone</p>
              <p class="${PROSE} text-sm m-0">
                No outline, shadow, rotation or stretch, and no straightening of the lean. One ink
                colour at a time, which is either of the two in the files or the ink of the surface
                it sits on.
              </p>
            </div>
          </div>
        </div>
      `,
    })}

    ${section({
      id: 'colour',
      heading: 'Colour',
      layout: 'split',
      lede: html`
        Neutrals carry everything. The green is rationed to the primary action and to live state,
        and it never tints a panel, never colours a heading, and never appears as a gradient.
        Each value is given for the light theme first and the dark theme second.
      `,
      body: html`
        <ul class="m-0 p-0 list-none grid gap-px bg-rule border border-rule rounded overflow-hidden mid:grid-cols-2">
          ${PALETTE.map(
            (s) => html`
              <li class="bg-paper-elev p-4 flex items-center gap-4">
                <span class="sw-${s.token} block size-12 shrink-0 rounded-sm border border-rule-strong"></span>
                <span class="flex flex-col gap-1 min-w-0">
                  <span class="font-mono text-sm font-semibold">${s.token}</span>
                  <span class="text-sm text-ink-muted">${s.role}</span>
                  <span class="font-mono text-xs text-ink-subtle">${s.light} ${s.dark}</span>
                </span>
              </li>
            `,
          )}
        </ul>
      `,
    })}

    ${section({
      id: 'type',
      heading: 'Type',
      body: html`
        <div class="grid gap-10 mid:grid-cols-2">
          <div>
            <p class="font-sans font-bold text-h2 leading-[1.05] tracking-tight m-0">
              From sandbox to production, on the same URL.
            </p>
            <p class="${PROSE} text-sm m-0 mt-4">
              Headings and prose are the reader's own system sans, bold for headings with the
              tracking pulled in. No font is downloaded, so the page has its type before the first
              byte of anything else arrives.
            </p>
          </div>
          <div>
            <p class="font-mono font-semibold text-h2 leading-[1.05] tracking-tight m-0">pilot exec</p>
            <p class="${PROSE} text-sm m-0 mt-4">
              The system monospace sets the name beside the mark, field labels, measured numbers
              and every terminal. It is the voice of the instrument panel, and it is kept for
              things a machine would print.
            </p>
          </div>
        </div>
      `,
    })}

    ${section({
      id: 'name',
      heading: 'Writing the name',
      layout: 'split',
      lede: html`
        Beside the mark the name is set in the monospace at semibold, lowercase, at the size shown
        in the header below. In a sentence it is Pilots, with a capital and in no special type.
      `,
      body: html`
        <div class="flex flex-col gap-8">
          ${headerMock(DELTA)}

          <dl class="m-0 border-t border-rule">
            ${NAME_FORMS.map(
              (n) => html`
                <div class="grid gap-x-6 gap-y-1 py-4 border-b border-rule mid:grid-cols-[8rem_5rem_1fr] mid:items-baseline">
                  <dt class="font-mono font-semibold ${n.ok ? 'text-ink' : 'text-ink-subtle line-through'}">${n.form}</dt>
                  <dd class="m-0 ${FIELD_LABEL}">${n.ok ? 'use' : 'avoid'}</dd>
                  <dd class="m-0 text-sm text-ink-muted">${n.note}</dd>
                </div>
              `,
            )}
          </dl>
        </div>
      `,
    })}

    <div id="usage" class="scroll-mt-24 max-w-6xl mx-auto px-6 pb-24">
      <div class="border-t border-rule pt-10 grid gap-8 mid:grid-cols-[14rem_1fr]">
        <h2 class="text-h3 font-bold m-0">Using the mark</h2>
        <div class="flex flex-col gap-4">
          <p class="${PROSE} m-0">
            Use the mark and the name freely to refer to Pilots. That covers an article, a talk, a
            comparison, documentation for an integration, and a note that something runs on it.
            Nobody needs to ask for any of those.
          </p>
          <p class="${PROSE} m-0">
            The source code is under the Apache licence, and that licence does not extend to the
            name or the mark. Ask before using either as part of another product's name or logo,
            on merchandise, or in a way that suggests Pilots endorses something.
            <a class=${LINK} href="${GH_URL}/issues" target="_blank" rel="noopener"
              >Open an issue to ask${NEW_TAB}</a
            >.
          </p>
        </div>
      </div>
    </div>
  `;
}
