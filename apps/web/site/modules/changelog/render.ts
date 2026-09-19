import { html } from '@webjsdev/core';
import { LINK } from '#site/lib/design/recipes.ts';
import { parseInline } from '#site/modules/changelog/parse.ts';
import type { Block, Inline } from '#site/modules/changelog/parse.ts';

/**
 * An entry's body as templates, never as an HTML string. The files are
 * written by hand and reviewed, so this is not a defence against an attacker
 * so much as against an angle bracket in a sentence about generics: every
 * piece of text goes through the template's own escaping and there is no
 * `unsafeHTML` on the page to get wrong.
 */
function inline(text: string) {
  return renderInlines(parseInline(text));
}

function renderInlines(parts: Inline[]): unknown[] {
  return parts.map((part) => {
    if (part.kind === 'code') return html`<code class="text-[0.9em]">${part.text}</code>`;
    if (part.kind === 'strong') return html`<strong class="font-semibold text-ink">${renderInlines(part.parts)}</strong>`;
    if (part.kind === 'link') {
      return part.href.startsWith('/')
        ? html`<a class=${LINK} href=${part.href}>${part.text}</a>`
        : html`<a class=${LINK} href=${part.href} target="_blank" rel="noopener">${part.text}</a>`;
    }
    return part.text;
  });
}

export function renderBlocks(blocks: Block[]) {
  return blocks.map((block) => {
    if (block.kind === 'heading') {
      return html`<h3 class="text-base font-semibold m-0 mt-8 first:mt-0 mb-3">${block.text}</h3>`;
    }
    if (block.kind === 'para') {
      return html`<p class="text-ink-muted leading-[1.7] m-0 mb-4 max-w-[68ch]">${inline(block.text)}</p>`;
    }
    return html`
      <ul class="list-disc pl-5 m-0 mb-4 max-w-[68ch] text-ink-muted leading-[1.7] marker:text-ink-subtle">
        ${block.items.map(
          (paras) => html`
            <li class="mb-2.5">
              ${paras.length === 1
                ? inline(paras[0])
                : paras.map((p) => html`<p class="m-0 mb-2 last:mb-0">${inline(p)}</p>`)}
            </li>
          `,
        )}
      </ul>
    `;
  });
}
