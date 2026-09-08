/**
 * A machine's state, as a word with a dot.
 *
 * Three surfaces render it (the live list, the machine page, a service's
 * previews) and they have to agree, because a reader learns the colours once.
 * Colour is never the only signal: the word carries the meaning and the dot
 * only repeats it, which is why the dot is `aria-hidden` and the word is not.
 *
 * A word rather than the engine's own value. `suspended` in a status column
 * teaches a reader that this product leaks its engine; `Sleeping` teaches them
 * what is happening. `stateLabel` in `lib/vocabulary.ts` owns the mapping and
 * answers `Unknown` for anything it has not been taught, so a state nobody
 * here recognises is never printed raw.
 */
import { html } from '@webjsdev/core';
import type { TemplateResult } from '@webjsdev/core';
import { stateLabel } from '#lib/vocabulary.ts';
import type { Tone } from '#lib/vocabulary.ts';

/** One token colour per tone. Written out so Tailwind's scanner sees them. */
const DOT: Record<Tone, string> = {
  success: 'bg-success',
  warning: 'bg-warning',
  muted: 'bg-muted-foreground',
  destructive: 'bg-destructive',
};

export function statusDot(state: string): TemplateResult {
  const { word, tone } = stateLabel(state);
  return html`<span class="inline-flex items-center gap-1.5 whitespace-nowrap">${toneDot(tone)}${word}</span>`;
}

/**
 * The dot alone, for a line whose words are not a machine state: an app's
 * `2/2 services online`. Decorative, so hidden from assistive tech; the text
 * beside it carries the meaning.
 */
export function toneDot(tone: Tone): TemplateResult {
  return html`<span aria-hidden="true" class=${`inline-block size-1.5 shrink-0 rounded-full ${DOT[tone]}`}></span>`;
}
