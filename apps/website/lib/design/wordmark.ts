import { html } from '@webjsdev/core';
import { DELTA_D } from '#lib/design/delta-p.ts';
import {
  PILOTS_MIXED_D,
  PILOTS_MIXED_W,
  PILOTS_UPPER_D,
  PILOTS_UPPER_W,
  PILOTS_DOTLESS_D,
  PILOTS_DOTLESS_W,
  DOTLESS_I_X,
  DOTLESS_I_ADV,
} from '#lib/design/wordmark-outlines.ts';

/**
 * The Pilots wordmark: the whole name as outlined type, then treated.
 *
 * TWO THINGS WERE LEARNED THE HARD WAY GETTING HERE, and both are the reason
 * this file looks the way it does.
 *
 * FIRST, THE LETTERS ARE NOT DRAWN BY HAND. An earlier round built the six
 * letters out of arcs and bars on a grid. A compass-drawn S and a squared O are
 * exactly where amateur lettering announces itself, and the result read as
 * funky rather than as uniform. The sibling brand's own lockup does not do
 * this either: everything after its W is the typeface's glyphs converted to
 * outlines. So the letters here come from a face somebody spent years drawing,
 * and this file only decides what happens to them.
 *
 * SECOND, THE P IS NOT THE DELTA. The round after that tried to build a capital
 * P out of the mark, on the theory that the sibling's W is drawn geometry. It
 * fails for a reason worth writing down: a P is read from its COUNTER, the
 * enclosed hole in the bowl, and the delta is a solid triangle with no counter
 * to give. A triangle against a stem reads as a play button, so the lockup said
 * "▶ilots". No amount of turning or scaling fixes a missing counter, and a W is
 * the opposite case, being all strokes and no counter at all, which is why the
 * same move works there and not here.
 *
 * WHAT ACTUALLY DISTINGUISHES THE WORD, then, is not a redrawn letter. It is
 * the treatment applied across all six of them, and the mark's own horizon is
 * the thing to carry: it is the one feature no other triangle has, so extending
 * it is what makes the letters belong to this mark rather than to any mark.
 * Everything below is one of those treatments.
 */

const ACCENT_INK = 'var(--logo-accent, currentColor)';

/* The grid the outlines were emitted on, so nothing has to be rescaled. */
const CAP = 100;

/* The mark's own 32-unit grid, copied from delta-p.ts so the transform nests
   identically. The path is imported: the mark is never redrawn. */
const D_CX = 16.035;
const D_CY = 15.7;
const D_W = 21.6;
const D_H = 23.8;
/** The mark cuts itself at y 17.6 to 19.6 of its 32 units. Read, not chosen. */
const BAND_TOP = 17.6;
const BAND_BOT = 19.6;

export type Wordmark = {
  id: string;
  name: string;
  idea: string;
  cost: string;
  /** Which setting of the name to use. */
  word: 'mixed' | 'upper' | 'dotless';
  /** The mark standing to the left of the word, at cap height. */
  lead?: boolean;
  /** The mark serving as the dot of the i. Requires the dotless setting. */
  tittle?: boolean;
  /** The mark's band, carried across everything. */
  horizon?: boolean;
};

/** The mark, filled, its ink centre landing on the point given. */
function delta(cx: number, cy: number, s: number) {
  return html`
    <g transform="translate(${cx} ${cy}) scale(${s}) translate(${-D_CX} ${-D_CY})">
      <g transform="translate(5.4 0)">
        <g transform="skewX(-11)"><path d=${DELTA_D} fill=${ACCENT_INK} /></g>
      </g>
    </g>
  `;
}

/** The whole lockup, at a given cap height in pixels. */
export function wordmarkLockup(v: Wordmark, px: number) {
  const wordD =
    v.word === 'upper' ? PILOTS_UPPER_D : v.word === 'dotless' ? PILOTS_DOTLESS_D : PILOTS_MIXED_D;
  const wordW =
    v.word === 'upper' ? PILOTS_UPPER_W : v.word === 'dotless' ? PILOTS_DOTLESS_W : PILOTS_MIXED_W;

  /* The leading mark is set to cap height and given a gap of a quarter of it,
     which is about the space the face itself leaves between two words. */
  const leadS = v.lead ? CAP / D_H : 0;
  const leadW = v.lead ? D_W * leadS : 0;
  const leadGap = v.lead ? CAP * 0.3 : 0;
  const wordX = leadW + leadGap;
  const totalW = wordX + wordW;

  /* The tittle is the mark shrunk to the width of the i's own stem slot and
     dropped just above the x-height, which is where the dot it replaces sat.
     Both numbers come from the generated slot rather than from the eye. */
  const tS = (DOTLESS_I_ADV * 1.15) / D_W;
  const tCx = wordX + DOTLESS_I_X + DOTLESS_I_ADV / 2;
  const tCy = 12;

  /* Where the band lands. On the leading mark it is the mark's own cut, so the
     two agree by construction rather than by a number chosen to match. */
  const bandTop = v.lead ? (BAND_TOP - 3.8) * leadS : CAP * 0.55;
  const bandH = v.lead ? (BAND_BOT - BAND_TOP) * leadS : CAP * 0.085;

  const top = -22;
  const totalH = CAP - top + 10;
  const h = (px * totalH) / CAP;
  return html`
    <svg
      viewBox="0 ${top} ${totalW.toFixed(1)} ${totalH.toFixed(1)}"
      height=${h.toFixed(1)}
      width=${((h * totalW) / totalH).toFixed(1)}
      role="img"
      aria-label="Pilots"
      class="block shrink-0"
    >
      ${v.lead ? delta(leadW / 2, CAP / 2, leadS) : ''}
      <g transform="translate(${wordX.toFixed(1)} 0)">
        <path d=${wordD} fill="currentColor" />
      </g>
      ${v.tittle ? delta(tCx, tCy, tS) : ''}
      <!-- The band, cut in the paper colour exactly as the mark cuts its own,
           run the full width so it reads as one horizon rather than as separate
           nicks. Drawn last so it crosses the letters and the mark alike. -->
      ${v.horizon
        ? html`<rect
            x=${-totalW * 0.02}
            y=${bandTop.toFixed(1)}
            width=${totalW * 1.04}
            height=${bandH.toFixed(1)}
            fill="var(--logo-bg)"
          />`
        : ''}
    </svg>
  `;
}

export const WORDMARKS: Wordmark[] = [
  {
    id: 'plain',
    name: 'The name, outlined',
    word: 'mixed',
    lead: true,
    idea: 'The control, and the thing every other card is measured against. Mark at cap height, a word space, then the name as outlined paths. No treatment at all.',
    cost: 'Nothing here could not be reproduced by anyone with the same typeface. It is a setting rather than a mark, which is the whole objection this section exists to answer.',
  },
  {
    id: 'horizon',
    name: 'One horizon',
    word: 'mixed',
    lead: true,
    horizon: true,
    idea: "The mark's own band run straight through the mark and the six letters at one height. The cut is the only feature no other triangle has, so extending it is what makes these letters belong to this mark and not to any mark.",
    cost: 'A band across type is a device a reader has to accept, and at small sizes it eats a real fraction of every letter. It also has to be redrawn for any ground that is not a flat colour.',
  },
  {
    id: 'tittle',
    name: 'The mark is the dot',
    word: 'dotless',
    tittle: true,
    idea: 'No lockup at all. The i gives up its dot and the mark takes the slot, so the name carries its own logo inside itself and needs nothing standing beside it.',
    cost: 'It only works where the whole word is shown. There is no short form, and the mark has to be lifted back out of the word for a favicon.',
  },
  {
    id: 'tittle-horizon',
    name: 'The dot, and the horizon',
    word: 'dotless',
    tittle: true,
    horizon: true,
    idea: 'Both moves at once. The mark sits in the word as the dot of the i, and the band it carries runs across every letter, so the two devices explain each other rather than competing.',
    cost: 'Two devices on six letters is close to the limit. The band also passes just under the tittle, which is the one place the drawing gets busy.',
  },
  {
    id: 'upper-horizon',
    name: 'All caps, cut',
    word: 'upper',
    lead: true,
    horizon: true,
    idea: 'The same horizon against caps, tracked wide. Caps give the lockup one flat line along the top for the band to answer, which is the rhythm both reference wordmarks are built on.',
    cost: 'Caps make the name noticeably longer, and at header width that is the first thing that has to give.',
  },
];
