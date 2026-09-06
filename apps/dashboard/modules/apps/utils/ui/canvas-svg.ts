/**
 * The two drawings of an app's layout: the edges behind the canvas cards, and
 * the thumbnail on the app card.
 *
 * Both are plain SVG the server emits from `layoutApp`'s output, so the
 * picture exists at first paint and with scripting off, and both use the
 * layout's own width and height as their viewBox: the thumbnail is the canvas
 * at a smaller size, not a second drawing of it.
 *
 * Colours are Tailwind token utilities (`stroke-border-strong`, `fill-card`),
 * so both drawings follow the theme the way every other surface does.
 */

import { html } from '@webjsdev/core';
import type { TemplateResult } from '@webjsdev/core';
import { CARD } from '#modules/apps/utils/layout.ts';
import type { AppLayout, PlacedNode } from '#modules/apps/utils/layout.ts';

/** A dashed line from the dependent's top centre up to the dependency's bottom centre. */
function edgeLine(from: PlacedNode, to: PlacedNode, marker: string): TemplateResult {
  return html`<line
    x1=${from.x + CARD.w / 2}
    y1=${from.y}
    x2=${to.x + CARD.w / 2}
    y2=${to.y + CARD.h}
    stroke-width="2"
    stroke-dasharray="6 6"
    marker-end=${`url(#${marker})`}
  ></line>`;
}

/**
 * The arrows, drawn once behind the cards.
 *
 * `aria-hidden`: the same relationship is in the text of every card's
 * footer and in the panel, so the picture adds nothing a screen reader needs
 * announced twice.
 */
export function edgesSvg(layout: AppLayout): TemplateResult {
  const at = new Map(layout.placed.map((p) => [p.id, p] as const));
  const marker = 'canvas-arrow';
  return html`<svg
    viewBox=${`0 0 ${layout.width} ${layout.height}`}
    class="absolute inset-0 size-full stroke-border-strong"
    aria-hidden="true"
  >
    <defs>
      <marker id=${marker} viewBox="0 0 10 10" refX="8" refY="5" markerWidth="6" markerHeight="6" orient="auto-start-reverse">
        <path d="M 0 0 L 10 5 L 0 10 z" class="fill-border-strong" stroke="none"></path>
      </marker>
    </defs>
    ${layout.edges.map((e) => {
      const from = at.get(e.from);
      const to = at.get(e.to);
      return from && to ? edgeLine(from, to, marker) : '';
    })}
  </svg>`;
}

const plural = (n: number, word: string) => `${n} ${word}${n === 1 ? '' : 's'}`;

/**
 * The app card's thumbnail: one rectangle per service in the canvas's own
 * positions, so the list and the canvas read as one object at two zoom
 * levels. `role="img"` with a name that says what it shows, because a picture
 * of rectangles has no other accessible meaning.
 */
export function thumbnailSvg(layout: AppLayout): TemplateResult {
  const at = new Map(layout.placed.map((p) => [p.id, p] as const));
  const label = `${plural(layout.placed.length, 'service')}, ${plural(layout.edges.length, 'connection')}`;
  return html`<svg
    viewBox=${`0 0 ${Math.max(layout.width, CARD.w)} ${Math.max(layout.height, CARD.h)}`}
    width="160"
    height="96"
    role="img"
    aria-label=${label}
    class="mx-auto block stroke-border-strong"
  >
    ${layout.edges.map((e) => {
      const from = at.get(e.from);
      const to = at.get(e.to);
      return from && to
        ? html`<line
            x1=${from.x + CARD.w / 2}
            y1=${from.y}
            x2=${to.x + CARD.w / 2}
            y2=${to.y + CARD.h}
            stroke-width="4"
            stroke-dasharray="12 12"
          ></line>`
        : '';
    })}
    ${layout.placed.map(
      (p) => html`<rect x=${p.x} y=${p.y} width=${CARD.w} height=${CARD.h} rx="16" stroke-width="4" class="fill-card stroke-border"></rect>`,
    )}
  </svg>`;
}
