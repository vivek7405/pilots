/**
 * Progress: progress bar. Tier-1 class helper over the native
 * `<progress>` element. The native element supplies the `progressbar`
 * role and `aria-valuenow` automatically from its `value` / `max`
 * attributes, so no JS, no custom element.
 *
 * shadcn parity:
 *   Progress  → progressClass()  (visual: 2px track + animated fill,
 *                                 styled via `::-webkit-progress-bar`,
 *                                 `::-webkit-progress-value`, and
 *                                 `::-moz-progress-bar`)
 *
 * A11y (required for accessible output): give the <progress> an accessible
 * name with aria-label (or a <label for>), e.g. aria-label="Upload
 * progress". The native element supplies the role and value; only the name
 * is the author's responsibility.
 *
 * Design tokens used: --primary.
 *
 * Full usage example: npx @webjsdev/ui view progress  (or the MCP tool: ui progress)
 */

/**
 * Class for the native `<progress>` element. The track + fill colours
 * come from Tailwind utilities. The browser handles the actual bar
 * rendering through the `::-webkit-progress-value` and
 * `::-moz-progress-bar` pseudo-elements. We expose them via the
 * `[&::-webkit-progress-value]:bg-primary` and
 * `[&::-moz-progress-bar]:bg-primary` Tailwind variants.
 *
 * Indeterminate state (no `value` attribute on the element) gets a
 * pulse animation via `:indeterminate { animate-pulse }`.
 */
// One string literal rather than an array joined at call time. The kit ships
// the array form; the elision pass reads a call in a module-level initialiser
// as work the browser might need, which pins every page that imports this file
// and un-elides every display-only component on it. The compiled classes are
// identical.
//
//  - block h-2 w-full overflow-hidden rounded-full : the track's own box.
//  - appearance-none + the ::-webkit-progress-bar rule : strip the UA's 3D bar.
//  - the ::-webkit-progress-value and ::-moz-progress-bar rules : the fill, in
//    both engines, since neither exposes the other's pseudo-element.
//  - indeterminate:animate-pulse : a <progress> with no value has no bar to
//    colour, so the track pulses instead.
export const progressClass = (): string =>
  'block h-2 w-full overflow-hidden rounded-full appearance-none border-0 bg-primary/20 ' +
  '[&::-webkit-progress-bar]:bg-primary/20 [&::-webkit-progress-value]:bg-primary ' +
  '[&::-webkit-progress-value]:transition-all [&::-moz-progress-bar]:bg-primary indeterminate:animate-pulse';
