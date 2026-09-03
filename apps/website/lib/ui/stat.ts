import { html } from '@webjsdev/core';
import { FACTS, type FactKey } from '#lib/facts.ts';
import { FIELD_LABEL } from '#lib/design/recipes.ts';

/**
 * The ONLY way a number reaches the page.
 *
 * AGENTS.md invariant 1 requires every digit-bearing claim to sit inside a
 * <data data-source="…"> element, and test/no-slop/no-slop.test.ts enforces
 * it. Routing every number through one helper means the provenance cannot be
 * dropped by an edit: there is no path to rendering a number without it.
 *
 * The data-source is not decoration either. It is in the served HTML, so a
 * reader who wants to know where "<1s" came from can read it out of view-source
 * without asking anyone.
 */

/** A number at instrument size, with its label and provenance. */
export function readout(key: FactKey) {
  const f = FACTS[key];
  return html`
    <div class="flex flex-col gap-1">
      <data
        class="font-mono text-readout font-semibold leading-none tracking-tight"
        value=${f.value}
        data-source=${f.source}
        title=${f.source}
      >${f.value}</data>
      <span class="${FIELD_LABEL}">${f.label}</span>
    </div>
  `;
}

/** The same fact inline, for use inside a sentence. */
export function inlineFact(key: FactKey) {
  const f = FACTS[key];
  return html`<data
    class="font-mono font-semibold text-ink"
    value=${f.value}
    data-source=${f.source}
    title=${f.source}
  >${f.value}</data>`;
}
