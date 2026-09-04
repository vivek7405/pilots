/**
 * Checkbox: styled native `<input type="checkbox">`. Tier-1 class
 * helper. Uses `appearance: none` + an inline-SVG `background-image` for
 * the checkmark when `:checked`, so it remains a real form control
 * (participates in `<form>` submission, no ElementInternals required).
 *
 * shadcn parity:
 *   Checkbox  → checkboxClass()  (visual: size-4, rounded, primary fill
 *                                 when checked, inset shadow, focus ring;
 *                                 bypasses Radix)
 *
 * Design tokens used: --input, --primary, --primary-foreground, --background,
 * --ring, --destructive.
 *
 * A11y (required for accessible output):
 *   `data-slot="checkbox"` is REQUIRED, not decoration. The injected stylesheet
 *   keys the checkmark on it, so without it a checked box renders as a filled
 *   square with no tick: the checked and unchecked states then differ by colour
 *   alone, which is exactly what WCAG 1.4.1 rules out. Every example here
 *   carries it; keep it when you copy one.
 *   Associate a label with the input, either `<label for>` pointing at the
 *   input's `id` (as below) or by nesting the input inside the `<label>`. A
 *   checkbox with no label has no accessible name.
 *   Group related checkboxes in a `<fieldset>` with a `<legend>` naming the
 *   group, so the set is announced as one thing rather than N loose controls.
 *   Set `aria-invalid="true"` on a failed checkbox (the class styles it) and
 *   point `aria-describedby` at the element holding the error text.
 *   The indeterminate state is a PROPERTY, not an attribute
 *   (`el.indeterminate = true`). Do NOT add `aria-checked="mixed"` alongside it:
 *   the browser already maps a native checkbox's `indeterminate` to a mixed
 *   checked state, and hand-writing the ARIA duplicates a state the host
 *   language owns, which is discouraged precisely because the two can then
 *   disagree. Set the property and leave the ARIA alone.
 *
 * Full usage example: npx @webjsdev/ui view checkbox  (or the MCP tool: ui checkbox)
 */
import { cn } from '../../lib/utils/cn.ts';

// The checkmark and indeterminate-dash CSS is NOT injected from here. It lives
// in the theme stylesheet (`public/input.css`), for the same two reasons
// `native-select.ts` keeps its `<option>` rule there:
//
//  1. A module-scope `installCheckboxStyles()` call is client work to the
//     elision analyser (#1320), so every page importing this module would ship
//     its whole JavaScript bundle. The pages here render forms and tables and
//     otherwise hydrate nothing, so that would be the only reason any of them
//     shipped at all.
//  2. A rule that arrives from a `<style>` the browser appends after load is
//     not in the first paint and does nothing with JavaScript off. The
//     checkmark is the ONLY thing distinguishing checked from unchecked other
//     than colour (WCAG 1.4.1), so it has to be in the stylesheet.
//
// Keep the two in sync: the rule keys on `data-slot="checkbox"`, which every
// call site below is still required to set.

const CHECKBOX_CLASS =
  'peer size-4 shrink-0 appearance-none rounded-[4px] border border-input bg-transparent shadow-xs transition-shadow outline-none focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50 disabled:cursor-not-allowed disabled:opacity-50 aria-invalid:border-destructive aria-invalid:ring-destructive/20 checked:border-primary checked:bg-primary checked:bg-no-repeat checked:bg-center dark:bg-input/30 dark:aria-invalid:ring-destructive/40';

/**
 * Tailwind classes for a styled native `<input type="checkbox">`.
 *
 * PAIR THIS WITH `data-slot="checkbox"` ON THE SAME INPUT. The theme
 * stylesheet keys the checkmark and the indeterminate dash on that attribute,
 * so the class alone gives you a box that fills with colour when checked but
 * never draws a tick, leaving the two states distinguishable by colour only.
 */
export function checkboxClass(): string {
  return cn(CHECKBOX_CLASS);
}
