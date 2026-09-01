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
 * Two layouts, and the choice is not cosmetic. Every section on this site
 * once opened heading-then-lede-then-body, all twenty-three of them, which is
 * the same template rhythm the eyebrow was removed for: strip the kicker and a
 * three-part stack is still a three-part stack. `split` puts the lede beside
 * the heading instead of under it, so a reader scrolling the page meets two
 * different shapes rather than one repeated. Alternate them by what the
 * section is doing, and drop the lede entirely where the body speaks for
 * itself.
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
   * `stacked` puts the lede under the heading, `split` puts it beside.
   * Defaults to stacked. Vary it: a page where every section picks the same
   * one has only moved the template, not removed it.
   */
  layout?: 'stacked' | 'split';
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
        ${opts.layout === 'split' && opts.lede
          ? html`
              <div class="grid gap-6 wide:grid-cols-[1fr_1fr] wide:gap-14 wide:items-end">
                <h2 class="${H2} max-w-[18ch]">${opts.heading}</h2>
                <p class="${PROSE} m-0 wide:pb-1">${opts.lede}</p>
              </div>
            `
          : html`
              <h2 class="${H2}">${opts.heading}</h2>
              ${opts.lede ? html`<p class="${PROSE} mt-4 text-lede">${opts.lede}</p>` : ''}
            `}
        <div class="mt-10">${opts.body}</div>
      </div>
    </section>
  `;
}
