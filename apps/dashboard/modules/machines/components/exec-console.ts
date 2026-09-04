/**
 * One command per submit, over the exec stream.
 *
 * Not a terminal: there is no PTY behind it and `stdin` is off, so a command
 * that waits for input would hang with no way to interrupt it. What this is for
 * is the thing a terminal is usually opened for anyway -- run something, read
 * what it printed, see the exit code.
 *
 * Output is appended to a container rendered once, for the same reason the log
 * pane does it: a re-render would wipe what already streamed in.
 */

import { WebComponent, html, prop, connectWS } from '@webjsdev/core';
import { createRef, ref } from '@webjsdev/core/directives';
import { buttonClass } from '#components/ui/button.ts';
import { inputClass } from '#components/ui/input.ts';
import { cn } from '#lib/utils/cn.ts';

/** Same surface as the log pane above it, so the two read as one console. */
const OUTPUT = 'mt-2 h-56 overflow-auto rounded-md border border-border bg-muted p-3 text-xs font-mono whitespace-pre-wrap';

interface Frame {
  type: 'stdout' | 'stderr' | 'exit' | 'error';
  data?: string;
  code?: number;
  message?: string;
}

class ExecConsole extends WebComponent({
  machineId: prop(String, { attribute: 'machine-id' }),
  running: prop(Boolean, { state: true }),
  lastExit: prop(String, { state: true }),
}) {
  private out = createRef<HTMLPreElement>();
  private input = createRef<HTMLInputElement>();
  private conn: { send(data: unknown): void; close(): void } | null = null;

  constructor() {
    super();
    this.machineId = '';
    this.running = false;
    this.lastExit = '';
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    this.conn?.close();
    this.conn = null;
  }

  private run(event: Event) {
    event.preventDefault();
    const command = this.input.value?.value.trim();
    if (!command || this.running || !this.machineId) return;

    this.running = true;
    this.lastExit = '';
    this.write(`$ ${command}\n`);

    this.conn = connectWS(`/api/machines/${this.machineId}/exec`, {
      onOpen: () => this.conn?.send({ cmd: ['sh', '-c', command] }),
      onMessage: (frame: Frame) => this.onFrame(frame),
      onClose: () => {
        this.running = false;
        this.conn = null;
      },
    });
  }

  private onFrame(frame: Frame) {
    if (frame.type === 'stdout' || frame.type === 'stderr') {
      this.write(atob(frame.data ?? ''));
      return;
    }
    if (frame.type === 'exit') {
      this.lastExit = String(frame.code ?? 0);
      this.write(`\n[exit ${frame.code ?? 0}]\n\n`);
      return;
    }
    if (frame.type === 'error') this.write(`\n[error: ${frame.message ?? 'unknown'}]\n\n`);
  }

  private write(text: string) {
    const out = this.out.value;
    if (!out) return;
    out.appendChild(document.createTextNode(text));
    out.scrollTop = out.scrollHeight;
  }

  render() {
    return html`
      <form class="flex gap-2" @submit=${(e: Event) => this.run(e)}>
        <label class="sr-only" for="exec-cmd">Command</label>
        <input
          ${ref(this.input)}
          id="exec-cmd"
          name="cmd"
          placeholder="ls -la"
          autocomplete="off"
          class=${cn(inputClass(), 'font-mono')}
        >
        <button type="submit" ?disabled=${this.running} class=${buttonClass()}>
          ${this.running ? 'Running' : 'Run'}
        </button>
      </form>
      <pre ${ref(this.out)} class=${cn(OUTPUT)} aria-live="polite" aria-label="Command output"></pre>
      <p class="text-xs text-muted-foreground">
        One command per run, as <code class="font-mono">sprite</code>, with stdin closed.
        ${this.lastExit ? html` Last exit code ${this.lastExit}.` : ''}
      </p>
    `;
  }
}

ExecConsole.register('exec-console');
