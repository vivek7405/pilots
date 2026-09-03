import { WebComponent, html, signal } from '@webjsdev/core';
import { WORKLOAD_APEX } from '#lib/links.ts';

/**
 * `<lifecycle-demo>`: drive one machine through its whole lifecycle and watch
 * the URL not change.
 *
 * This is the site's one substantial interactive element, and it exists to
 * make an argument rather than to decorate a section. "URLs are permanent" is
 * an architecture invariant (ARCHITECTURE.md rule 4) and it is the single
 * hardest thing to convey in prose, because every platform claims stable URLs
 * and most of them mean "stable until you redeploy". A reader who clicks
 * suspend, wake, checkpoint, restore, and promote, and watches one address sit
 * still through all five, has understood the claim in a way no sentence
 * achieves.
 *
 * It is a SIMULATION of the state machine, not a live API call, and it says so
 * on its face. Faking a network round-trip would be the fabricated-screenshot
 * problem in another costume (AGENTS.md invariant 9). What it simulates is
 * real: the states, the knobs, and the transitions are the ones in the
 * machines table.
 *
 * No timing numbers appear here on purpose. The transitions are instant
 * because nothing is happening; attaching "0.9s" to a fake wake would be an
 * unsourced claim, which invariant 1 forbids and the no-slop gate would fail.
 */

type State = 'running' | 'suspended' | 'stopped';
type Face = 'sandbox' | 'service';

type Entry = { verb: string; detail: string };

/** The machine name is fixed so the URL is visibly the same string throughout. */
const NAME = 'bold-otter';

export class LifecycleDemo extends WebComponent {
  state = signal<State>('running');
  face = signal<Face>('sandbox');
  /** How many lifecycle events this URL has survived. The point of the demo. */
  survived = signal(0);
  checkpoints = signal(0);
  log = signal<Entry[]>([{ verb: 'create', detail: 'machine created from template, URL assigned' }]);

  private record(verb: string, detail: string) {
    this.survived.set(this.survived.get() + 1);
    // Newest first, capped: an unbounded transcript grows the page under the
    // reader's cursor, which moves the buttons they are aiming at.
    this.log.set([{ verb, detail }, ...this.log.get()].slice(0, 6));
  }

  suspend() {
    if (this.state.get() !== 'running') return;
    this.state.set('suspended');
    this.record('suspend', 'memory snapshotted to S3, VM killed, slot released');
  }

  wake() {
    if (this.state.get() === 'running') return;
    this.state.set('running');
    this.record('wake', 'restored from S3 on whatever host answered, same URL');
  }

  checkpoint() {
    if (this.state.get() !== 'running') return;
    this.checkpoints.set(this.checkpoints.get() + 1);
    this.record('checkpoint', `named checkpoint #${this.checkpoints.get()}, uploaded in the background`);
  }

  restore() {
    if (this.checkpoints.get() === 0) return;
    this.state.set('running');
    this.record('restore', 'in-place: same machine row, same URL, same agent token');
  }

  promote() {
    if (this.face.get() === 'service') return;
    this.face.set('service');
    this.state.set('running');
    this.record('promote', 'now a production service with health checks and replicas');
  }

  reset() {
    this.state.set('running');
    this.face.set('sandbox');
    this.survived.set(0);
    this.checkpoints.set(0);
    this.log.set([{ verb: 'create', detail: 'machine created from template, URL assigned' }]);
  }

  private btn(label: string, on: () => void, enabled: boolean) {
    return html`
      <button
        class="h-8 px-3 rounded-[3px] border font-mono text-[12px] transition-colors duration-150
               ${enabled
                 ? 'border-rule-strong text-ink bg-paper-elev hover:bg-signal hover:text-signal-ink hover:border-signal cursor-pointer'
                 : 'border-rule text-ink-subtle bg-transparent cursor-not-allowed'}"
        ?disabled=${!enabled}
        @click=${on}
      >${label}</button>
    `;
  }

  render() {
    const state = this.state.get();
    const face = this.face.get();
    const running = state === 'running';
    const survived = this.survived.get();

    return html`
      <div class="rounded border border-rule bg-paper-elev overflow-hidden">
        <!-- The URL bar. Pinned at the top and never re-rendered with a
             different string: that IS the demonstration. -->
        <div class="px-4 py-3 border-b border-rule bg-paper-sunken flex items-center gap-3 flex-wrap">
          <span
            class="w-1.5 h-1.5 rounded-full shrink-0 ${running ? 'bg-signal live-dot' : 'bg-ink-subtle'}"
            aria-hidden="true"
          ></span>
          <span class="font-mono text-[13px] mid:text-sm text-ink truncate">${NAME}.${WORKLOAD_APEX}</span>
          <span
            class="ml-auto font-mono text-[10px] uppercase tracking-[0.14em] px-2 py-1 rounded-[2px]
                   ${face === 'service' ? 'bg-signal text-signal-ink' : 'border border-rule text-ink-subtle'}"
            >${face}</span
          >
        </div>

        <div class="grid mid:grid-cols-[1fr_1fr]">
          <!-- Left: the knobs that actually differ between the two faces. -->
          <div class="p-4 mid:border-r border-rule flex flex-col gap-3">
            <dl class="m-0 grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 font-mono text-[12px]">
              <dt class="text-ink-subtle">state</dt>
              <dd class="m-0 text-ink">${state}</dd>
              <dt class="text-ink-subtle">autoStop</dt>
              <dd class="m-0 text-ink">${face === 'sandbox' ? 'suspend' : 'off'}</dd>
              <dt class="text-ink-subtle">minRunning</dt>
              <dd class="m-0 text-ink">${face === 'sandbox' ? '0' : '1'}</dd>
              <dt class="text-ink-subtle">checkpoints</dt>
              <dd class="m-0 text-ink">${this.checkpoints.get()}</dd>
            </dl>

            <div class="flex flex-wrap gap-1.5 mt-1">
              ${this.btn('suspend', () => this.suspend(), running)}
              ${this.btn('wake', () => this.wake(), !running)}
              ${this.btn('checkpoint', () => this.checkpoint(), running)}
              ${this.btn('restore', () => this.restore(), this.checkpoints.get() > 0)}
              ${this.btn('promote', () => this.promote(), face === 'sandbox')}
            </div>

            <p class="m-0 text-[11px] text-ink-subtle leading-relaxed">
              A simulation of the state machine, not a live machine. The states, knobs,
              and transitions are the real ones.
            </p>
          </div>

          <!-- Right: the transcript, newest first. -->
          <div class="p-4 border-t mid:border-t-0 border-rule flex flex-col gap-2 min-h-[13rem]">
            <div class="flex items-baseline justify-between gap-3">
              <span class="font-mono text-[11px] uppercase tracking-[0.14em] text-ink-subtle">transcript</span>
              <button
                class="font-mono text-[11px] text-ink-subtle hover:text-ink underline underline-offset-2 bg-transparent border-0 cursor-pointer p-0"
                @click=${() => this.reset()}
              >reset</button>
            </div>
            <ol class="m-0 p-0 list-none flex flex-col gap-1.5">
              ${this.log.get().map(
                (e) => html`
                  <li class="font-mono text-[12px] leading-snug">
                    <span class="text-ink font-semibold">${e.verb}</span>
                    <span class="text-ink-muted"> ${e.detail}</span>
                  </li>
                `,
              )}
            </ol>
          </div>
        </div>

        <!-- The scoreboard. The whole point, stated as a running tally. -->
        <div class="px-4 py-3 border-t border-rule bg-paper-sunken flex items-center gap-3 flex-wrap">
          <span class="font-mono text-[11px] uppercase tracking-[0.14em] text-ink-subtle">URL changes</span>
          <span class="font-mono text-sm font-semibold text-ink">0</span>
          <span class="text-ink-subtle text-xs">across</span>
          <span class="font-mono text-sm font-semibold text-ink">${survived}</span>
          <span class="text-ink-subtle text-xs">
            lifecycle ${survived === 1 ? 'event' : 'events'}
          </span>
        </div>
      </div>
    `;
  }
}

LifecycleDemo.register('lifecycle-demo');
