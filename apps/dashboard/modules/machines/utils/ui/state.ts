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

/**
 * `default` for the one state that is serving traffic, `secondary` for the
 * resting states a wake brings back, `outline` for anything the engine reports
 * that this list has not been taught about.
 */
const VARIANTS: Record<string, BadgeVariant> = {
  running: 'default',
  starting: 'secondary',
  suspending: 'secondary',
  suspended: 'secondary',
  stopped: 'secondary',
  destroyed: 'outline',
  failed: 'destructive',
};

export function stateBadge(state: string): TemplateResult {
  return html`<span class=${cn(badgeClass({ variant: VARIANTS[state] ?? 'outline' }), 'font-mono')}>${state}</span>`;
}
