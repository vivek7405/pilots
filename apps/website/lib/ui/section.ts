import { html } from '@webjsdev/core';
import { H2, PROSE } from '#lib/design/recipes.ts';

/**
 * A section shell.
 *
 * Deliberately thin. AGENTS.md invariant 3 says sections are NOT a template:
 * if two sections could swap their markup unnoticed, at least one was not
 * designed. So this helper owns only the things that genuinely must be
 * identical everywhere (the vertical rhythm, the eyebrow/heading/lede
 * relationship, the anchor target) and takes the body as a slot. It does not
 * impose a grid, a column count, or an alignment, which is where a section
 * helper turns into the ten-identical-sections page it exists to prevent.
 *
 * There is NO eyebrow parameter, and its absence is the point. A small label
 * stacked above every heading is the single most recognisable generated-page
 * tell there is, and swapping the coloured pill for a monospace kicker does
 * not fix it: the tell is the STACK, not the styling. This file used to take
 * an eyebrow and every section passed one, which meant the page opened
 * eighteen blocks with the same three-part rhythm. A heading that needs a
 * label above it to make sense is a heading that needs rewriting.
 */
export function section(opts: {
  id: string;
  heading: string;
  /**
   * The opening sentence. It must resolve with nothing above it: readers
   * arrive mid-page from a search result or a shared link, so a lede that
   * opens on a backward reference ("That first paint", "All of it arrives")
   * leaves them stranded.
   */
  lede?: unknown;
  body: unknown;
}) {
  return html`
    <section id=${opts.id} class="scroll-mt-24 py-16 mid:py-24">
      <div class="max-w-6xl mx-auto px-6">
        <h2 class="${H2}">${opts.heading}</h2>
        ${opts.lede ? html`<p class="${PROSE} mt-4 text-lede">${opts.lede}</p>` : ''}
        <div class="mt-10">${opts.body}</div>
      </div>
    </section>
  `;
}
