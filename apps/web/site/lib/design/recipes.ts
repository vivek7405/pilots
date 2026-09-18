/**
 * Shared class recipes.
 *
 * Anything appearing on more than one page belongs here. A page needing a
 * one-off composes on top rather than restating the base: per-page copies of a
 * button string are exactly how a design system drifts, because a change lands
 * on whichever page the author happened to have open.
 *
 * The rules encoded below:
 *
 * - The signal green has TWO jobs, the primary action and live state, plus the
 *   focus ring. It never tints a panel, a heading, or a label (AGENTS.md
 *   invariant 2). Rationing it to the places that ask for a click is what keeps
 *   it meaningful rather than decorative.
 * - Signal green never carries TEXT colour on paper; it fails contrast there.
 *   It is a background with near-black type on it, or a hairline.
 * - Surfaces are square-ish. A 4px radius on a panel reads as a technical
 *   drawing; a 16px radius on the same panel reads as a consumer app. The
 *   pill on the primary button is the deliberate exception, so the one thing
 *   meant to be pressed is shaped differently from everything that is not.
 */

const BTN_BASE =
  'inline-flex items-center gap-2 h-10 px-5 rounded-full font-semibold text-sm leading-none ' +
  'no-underline border cursor-pointer transition-all duration-[140ms] whitespace-nowrap';

/** The single strongest action on a view. At most one per screen. */
export const BTN_PRIMARY =
  `${BTN_BASE} bg-signal text-signal-ink border-transparent hover:bg-signal-hover hover:-translate-y-px`;

/** Every other action. Reads as secondary without competing. */
export const BTN_GHOST =
  `${BTN_BASE} text-ink border-rule-strong bg-paper-elev hover:border-ink-subtle hover:bg-paper-subtle`;

/**
 * A content surface. Square-ish corners and a hairline rule, no drop shadow:
 * shadows imply a card floating over a page, and these are panels drawn ON
 * one. The distinction is what keeps the site from reading as a stack of
 * floating widgets.
 */
export const PANEL = 'rounded border border-rule bg-paper-elev';

/** A small monospace label. The instrument-panel voice for a field name. */
export const FIELD_LABEL =
  'font-mono text-[11px] uppercase tracking-[0.14em] text-ink-subtle';

/** A hairline horizontal rule used to separate sections without a heading. */
export const HAIRLINE = 'border-0 border-t border-rule';

/** Body copy inside a section. One measure, never full width. */
export const PROSE = 'text-ink-muted leading-[1.7] max-w-[62ch]';

/** The section heading. */
export const H2 = 'text-h2 font-bold m-0 tracking-tight';

/** A link in running prose. Underlined, because a colour-only link is invisible
 *  to a reader who cannot see the colour, and this palette has no blue. */
export const LINK =
  'text-ink underline decoration-rule-strong underline-offset-[3px] hover:decoration-ink transition-colors';
