/**
 * The console log, tailed.
 *
 * The container is rendered ONCE and appended to imperatively. A component that
 * re-rendered on every line would wipe the lines it had already streamed in,
 * so the line counter is a plain instance field that `render()` never reads.
 */

import { WebComponent, html, prop } from '@webjsdev/core';
import { createRef, ref } from '@webjsdev/core/directives';

class LogPane extends WebComponent({
  machineId: prop(String, { attribute: 'machine-id' }),
  status: prop(String, { state: true }),
}) {
  private pane = createRef<HTMLPreElement>();
  private controller: AbortController | null = null;
  /** A plain field, never a signal: reading it in render() would wipe the log. */
  private lines = 0;

  constructor() {
    super();
    this.machineId = '';
    this.status = 'idle';
  }

  connectedCallback() {
    super.connectedCallback();
    void this.tail();
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    this.controller?.abort();
    this.controller = null;
  }

  private async tail() {
    if (!this.machineId) return;
    this.controller = new AbortController();
    this.status = 'connecting';
    try {
      const res = await fetch(`/api/machines/${this.machineId}/logs`, { signal: this.controller.signal });
      if (!res.ok || !res.body) {
        this.status = `unavailable (${res.status})`;
        return;
      }
      this.status = 'live';

      const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
      let carry = '';
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        carry += value;
        const parts = carry.split('\n');
        carry = parts.pop() ?? '';
        for (const line of parts) this.appendLine(line);
      }
      if (carry) this.appendLine(carry);
      this.status = 'ended';
    } catch (err) {
      if ((err as Error).name !== 'AbortError') this.status = 'disconnected';
    }
  }

  private appendLine(line: string) {
    const pane = this.pane.value;
    if (!pane) return;
    // textContent, never innerHTML: a guest writes these bytes.
    pane.appendChild(document.createTextNode(line + '\n'));
    this.lines += 1;
    // Trim from the top rather than growing without limit; a chatty guest can
    // fill a tab's memory in minutes otherwise.
    while (this.lines > 2000 && pane.firstChild) {
      pane.removeChild(pane.firstChild);
      this.lines -= 1;
    }
    pane.scrollTop = pane.scrollHeight;
  }

  render() {
    return html`
      <div class="flex items-center justify-between text-xs text-muted-foreground mb-1">
        <span>Console</span><span>${this.status}</span>
      </div>
      <pre
        ${ref(this.pane)}
        class="h-72 overflow-auto rounded-md border border-border bg-muted p-3 text-xs font-mono whitespace-pre-wrap"
      ></pre>
    `;
  }
}

LogPane.register('log-pane');
