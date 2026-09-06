/**
 * A machine's state, as a badge.
 *
 * Three surfaces render it (the live list, the machine page, a service's PR
 * previews) and they have to agree, because a reader learns the colours once.
 * Colour is never the only signal: the state's own word is the label.
 */
import { html } from '@webjsdev/core';
import type { TemplateResult } from '@webjsdev/core';
import { badgeClass } from '#components/ui/badge.ts';
import type { BadgeVariant } from '#components/ui/badge.ts';
import { cn } from '#lib/utils/cn.ts';
import { stateLabel } from '#lib/vocabulary.ts';
import type { Tone } from '#lib/vocabulary.ts';

const DOT: Record<Tone, string> = {
  success: 'bg-success',
  warning: 'bg-warning',
  muted: 'bg-muted-foreground',
  destructive: 'bg-destructive',
};

/**
 * A machine's state as a word with a dot: `Online`, `Sleeping`, `Failed`.
 *
 * The word carries the meaning and the dot carries the tone, so a reader who
 * cannot tell the colours apart loses nothing. The word comes from the
 * vocabulary module, never from the engine's own string.
 */
export function statusDot(state: string): TemplateResult {
  const { word, tone } = stateLabel(state);
  return html`<span class="inline-flex items-center gap-1.5">
    ${toneDot(tone)}
    <span>${word}</span>
  </span>`;
}

/**
 * The dot alone, for a line whose words are not a machine state: an app's
 * `2/2 services online`. Decorative, so hidden from assistive tech; the text
 * beside it carries the meaning.
 */
export function toneDot(tone: Tone): TemplateResult {
  return html`<span class=${cn('inline-block size-1.5 shrink-0 rounded-full', DOT[tone])} aria-hidden="true"></span>`;
}

/**
 * `default` for the one state that is serving traffic, `secondary` for the
 * resting states a wake brings back, `outline` for anything the engine reports
 * that this list has not been taught about.
 */
const VARIANTS: Record<string, BadgeVariant> = {
  running: 'default',
  creating: 'secondary',
  starting: 'secondary',
  suspending: 'secondary',
  suspended: 'secondary',
  stopped: 'secondary',
  destroyed: 'outline',
  // Both spellings the engine can report. `error` is what a machine whose
  // Firecracker process died carries, and it was falling through to `outline`,
  // which reads as a state nobody has to look at.
  error: 'destructive',
  failed: 'destructive',
};

export function stateBadge(state: string): TemplateResult {
  return html`<span class=${cn(badgeClass({ variant: VARIANTS[state] ?? 'outline' }), 'font-mono')}>${state}</span>`;
}
