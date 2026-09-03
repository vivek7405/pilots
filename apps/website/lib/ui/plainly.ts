import { html } from '@webjsdev/core';

/**
 * The plain-language aside.
 *
 * The internals page is written for somebody who already knows what a page
 * fault is. That reader is the reason the page exists, and thinning the
 * technical prose out to accommodate everyone else would remove the only thing
 * that distinguishes it. So the plain version is ADDITIVE: it sits under the
 * dense explanation, in its own marked block, and can be skipped by the reader
 * who does not need it and read alone by the reader who does.
 *
 * Two rules this deliberately follows:
 *
 * - **The label is inline, on the same baseline as the sentence.** AGENTS.md
 *   invariant 12 bans a small monospace label STACKED above text, because that
 *   stack is the generated-page rhythm. A label that qualifies a value beside
 *   it is a different thing and stays legal, which is why this is one
 *   paragraph rather than a caption over a block.
 * - **It explains, it does not simplify away.** An analogy that makes the
 *   reader believe something false is worse than no analogy. Where the plain
 *   version has to drop a detail it says which detail it dropped, and the
 *   paragraph above it is still the authority.
 */
export function plainly(body: unknown) {
  return html`
    <aside class="mt-6 border-l-2 border-rule-strong pl-5">
      <p class="text-sm text-ink-muted leading-[1.7] m-0 max-w-[68ch]">
        <span class="font-mono text-[11px] uppercase tracking-[0.14em] text-ink-subtle mr-2"
          >In plain terms</span
        >${body}
      </p>
    </aside>
  `;
}
