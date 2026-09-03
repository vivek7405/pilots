import { html } from '@webjsdev/core';
import { PROSE } from '#lib/design/recipes.ts';

/**
 * The masthead for an interior page.
 *
 * Shared, unlike `section()`, and the difference is worth stating because
 * AGENTS.md invariant 3 forbids templated sections. A page's opening block is
 * chrome playing one role on every page, the way the header and footer are.
 * The SECTIONS below it are the content, and those stay hand-laid.
 *
 * Two columns rather than one. The home page can run a single narrow measure
 * because a terminal occupies the other half; an interior page has nothing
 * there, so a headline capped at ~16 characters per line left a third of the
 * viewport visibly empty and read as a layout bug rather than as restraint.
 * Splitting headline from lede fills the width and gives the eye somewhere to
 * go, and the baseline alignment is what keeps it from looking like two
 * unrelated blocks.
 *
 * No eyebrow, for the reason section() gives: a small label stacked above the
 * heading is the generated-page rhythm, and a monospace one is the same tell
 * in different clothes.
 */
export function pageHero(opts: {
  heading: string;
  lede: unknown;
  /** Optional actions, rendered under the lede. */
  actions?: unknown;
}) {
  return html`
    <div class="border-b border-rule">
      <div class="max-w-6xl mx-auto px-6 pt-14 pb-12 mid:pt-20 mid:pb-16">
        <div class="grid gap-7 wide:grid-cols-[1.05fr_0.95fr] wide:gap-14 wide:items-end">
          <h1 class="text-display font-bold leading-[1.02] m-0 max-w-[15ch]">${opts.heading}</h1>
          <div class="wide:pb-1.5">
            <p class="${PROSE} text-lede m-0">${opts.lede}</p>
            ${opts.actions ? html`<div class="flex flex-wrap gap-3 mt-7">${opts.actions}</div>` : ''}
          </div>
        </div>
      </div>
    </div>
  `;
}
