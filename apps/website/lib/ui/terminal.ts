import { html } from '@webjsdev/core';

/**
 * A terminal pane, server-rendered.
 *
 * NOTE: this file deliberately never writes the word w-i-n-d-o-w. The
 * framework's elision analyser tests the raw source (comments included)
 * against /\b(?:window|document|navigator|...)\b/ to decide whether a module
 * does client work. Prose describing "a terminal window" therefore marked this
 * module as client-effecting, and `webjs doctor` reported three static pages
 * shipping their whole page module to the browser because of a word in a
 * comment. Say "pane".
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

/**
 * @param title  The pane's label. Name what the transcript proves, not
 *               "Terminal": the label is read far more often than the body.
 */
export function terminal(title: string, lines: Line[]) {
  // Declared INSIDE the function, not at module scope. A module-scope object
  // literal counts as work done at import time, which makes this module a
  // client-effecting blocker and stops every page importing it from being
  // elided: three static pages were shipping their whole page module to the
  // browser to run one lookup table. `webjs doctor` is what surfaced it.
  const LINE_CLASS: Record<Line['kind'], string> = {
    cmd: 'text-ink',
    out: 'text-ink-muted',
    mark: 'text-ink font-semibold',
    note: 'text-ink-subtle italic',
  };
  return html`
    <div class="rounded border border-rule bg-paper-sunken overflow-hidden">
      <div class="flex items-center gap-2 px-3 h-9 border-b border-rule bg-paper-elev">
        <!-- A single hairline dash, not three coloured circles. The circles are
             a macOS pastiche every generated landing page ships, and the first
             pass here used two outlined squares which read as unticked
             checkboxes. One mark is enough to say "terminal". -->
        <span class="w-4 h-px bg-rule-strong shrink-0" aria-hidden="true"></span>
        <span class="font-mono text-[11px] text-ink-subtle truncate">${title}</span>
      </div>
      <pre class="m-0 p-4 overflow-x-auto scroll-thin font-mono text-[13px] leading-[1.75]"><code>${lines.map(
        (l) => html`<span class="block ${LINE_CLASS[l.kind]}">${
          l.kind === 'cmd' ? html`<span class="text-ink-subtle select-none">$ </span>` : ''
        }${l.text}</span>`,
      )}</code></pre>
    </div>
  `;
}
