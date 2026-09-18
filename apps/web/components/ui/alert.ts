/**
 * Alert: informational banner. Tier-1 class helpers; compose with a
 * native `<div role="alert">` (or `role="status"` for non-urgent updates).
 *
 * shadcn parity:
 *   Alert (variant: default | destructive)  → alertClass({ variant })
 *   AlertTitle                              → alertTitleClass()
 *   AlertDescription                        → alertDescriptionClass()
 *
 * A11y (required for accessible output): put role="alert" on the container
 * for an urgent, interrupting message, or role="status" for a polite,
 * non-urgent update. The class helper sets no role, so without one the
 * banner is silent to assistive tech.
 *
 * Design tokens used: --card, --card-foreground, --destructive, --muted-foreground.
 *
 * Full usage example: npx @webjsdev/ui view alert  (or the MCP tool: ui alert)
 */
import { cn } from '../../lib/utils/cn.ts';

const BASE =
  'relative grid w-full grid-cols-[0_1fr] items-start gap-y-0.5 rounded-lg border px-4 py-3 text-sm has-[>svg]:grid-cols-[calc(var(--spacing)*4)_1fr] has-[>svg]:gap-x-3 [&>svg]:size-4 [&>svg]:translate-y-0.5 [&>svg]:text-current';

const VARIANTS = {
  default: 'bg-card text-card-foreground',
  destructive:
    'bg-card text-destructive *:data-[slot=alert-description]:text-destructive/90 [&>svg]:text-current',
} as const;

export type AlertVariant = keyof typeof VARIANTS;

export function alertClass(opts: { variant?: AlertVariant } = {}): string {
  return cn(BASE, VARIANTS[opts.variant ?? 'default']);
}

export const alertTitleClass = (): string =>
  'col-start-2 line-clamp-1 min-h-4 font-medium tracking-tight';

export const alertDescriptionClass = (): string =>
  'col-start-2 grid justify-items-start gap-1 text-sm text-muted-foreground [&_p]:leading-relaxed';
