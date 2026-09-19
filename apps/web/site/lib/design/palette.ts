/**
 * The palette as the brand page prints it.
 *
 * The colours themselves are declared ONCE, in `public/site.input.css`, as
 * `light-dark()` pairs (AGENTS.md invariant 11). A brand page has to print the
 * hex values as text, which is a second copy, so `test/site/palette.test.ts`
 * reads the stylesheet and fails when a value here stops matching it. The
 * swatch itself is painted from the live token, never from these strings.
 */
export type Swatch = {
  /** The custom property's name, without the leading dashes. */
  token: string;
  /** What the colour is for, in a few words and with no full stop. */
  role: string;
  light: string;
  dark: string;
};

export const PALETTE: Swatch[] = [
  { token: 'paper', role: 'the page', light: '#f7f4ee', dark: '#0e1014' },
  { token: 'paper-elev', role: 'panels and cards', light: '#fffdf9', dark: '#15181e' },
  { token: 'ink', role: 'text and the mark', light: '#16181c', dark: '#e9e7e1' },
  { token: 'ink-muted', role: 'body prose', light: '#54585f', dark: '#9ba1ac' },
  { token: 'ink-subtle', role: 'captions and labels', light: '#80858e', dark: '#6b7280' },
  { token: 'rule', role: 'hairlines and borders', light: '#ddd7ca', dark: '#232830' },
  { token: 'signal', role: 'the one accent', light: '#a3e635', dark: '#b9f227' },
  { token: 'alert', role: 'errors only', light: '#d6482f', dark: '#ff7a5c' },
];
