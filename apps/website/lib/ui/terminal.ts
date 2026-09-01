import { html } from '@webjsdev/core';

/**
 * A terminal window, server-rendered.
 *
 * This is the site's hero artifact and its main illustration everywhere else,
 * which is a deliberate choice against the alternative: AGENTS.md invariant 9
 * bans the abstract 3D render and the fabricated product screenshot. A terminal
 * showing a command someone can actually run is the honest version of a hero
 * image, and it is also the interface this product is driven through.
 *
 * It is static markup, not a typing animation. An animated replay makes a
 * reader WAIT to read something they could already read, and the second visit
 * is worse than the first.
 */

export type Line =
  /** A command the reader could type. Rendered after a prompt mark. */
  | { kind: 'cmd'; text: string }
  /** Program output. */
  | { kind: 'out'; text: string }
  /** Output worth pulling the eye to: the result the example exists to show. */
  | { kind: 'mark'; text: string }
  /** A dimmed aside, for a note that is not part of the transcript. */
  | { kind: 'note'; text: string };

const LINE_CLASS: Record<Line['kind'], string> = {
  cmd: 'text-ink',
  out: 'text-ink-muted',
  mark: 'text-ink font-semibold',
  note: 'text-ink-subtle italic',
};

/**
 * @param title  The window's label. Name what the transcript proves, not
 *               "Terminal": the label is read far more often than the body.
 */
export function terminal(title: string, lines: Line[]) {
  return html`
    <div class="rounded border border-rule bg-paper-sunken overflow-hidden">
      <div class="flex items-center gap-2 px-3 h-9 border-b border-rule bg-paper-elev">
        <!-- Two hairline squares, not the three coloured circles. The circles
             are a macOS chrome pastiche that every generated landing page
             ships; these read as a technical window without borrowing anyone's
             UI. -->
        <span class="w-2 h-2 border border-rule-strong rounded-[1px]"></span>
        <span class="w-2 h-2 border border-rule-strong rounded-[1px]"></span>
        <span class="font-mono text-[11px] text-ink-subtle ml-1 truncate">${title}</span>
      </div>
      <pre class="m-0 p-4 overflow-x-auto scroll-thin font-mono text-[13px] leading-[1.75]"><code>${lines.map(
        (l) => html`<span class="block ${LINE_CLASS[l.kind]}">${
          l.kind === 'cmd' ? html`<span class="text-ink-subtle select-none">$ </span>` : ''
        }${l.text}</span>`,
      )}</code></pre>
    </div>
  `;
}
