/**
 * <build-log>: a build's output as it happens, and the deploy on its verdict.
 *
 * Rendered ONCE and appended to imperatively, like the log stream: a
 * component that re-rendered on every line would wipe the lines it had. It
 * reads `/api/builds/<id>/logs?follow=1` as NDJSON, appends `step` and `line`,
 * and keeps the last two thousand.
 *
 * With `autodeploy`, the terminal line carrying `result` (the image id) posts
 * the existing deploy route ONCE and then navigates to the new deployment.
 * A terminal `error` line becomes an alert with its code and nothing is
 * deployed. With scripting off the page shows the raw log link and the
 * Deploy form takes the image id by hand, so the verdict is never lost.
 */
import { WebComponent, html, prop, navigate } from '@webjsdev/core';
import { createRef, ref } from '@webjsdev/core/directives';
import type { Route } from '@webjsdev/core';
import { badgeClass } from '#components/ui/badge.ts';
import { cn } from '#lib/utils/cn.ts';

const PANE = 'h-72 overflow-auto rounded-md border border-border bg-muted p-3 text-meta font-mono whitespace-pre-wrap';

interface Line {
  step?: string;
  line?: string;
  error?: string;
  code?: string;
  result?: string;
}

export class BuildLog extends WebComponent({
  buildId: prop(String, { attribute: 'build-id' }),
  serviceId: prop(String, { attribute: 'service-id' }),
  autodeploy: prop(Boolean),
  status: prop(String, { state: true }),
  failure: prop(String, { state: true }),
}) {
  private pane = createRef<HTMLPreElement>();
  private controller: AbortController | null = null;
  private lines = 0;
  private deployed = false;

  constructor() {
    super();
    this.buildId = '';
    this.serviceId = '';
    this.autodeploy = false;
    this.status = 'connecting';
    this.failure = '';
  }

  connectedCallback() {
    super.connectedCallback();
    void this.follow();
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    this.controller?.abort();
    this.controller = null;
  }

  private async follow() {
    if (!this.buildId) return;
    this.controller = new AbortController();
    try {
      const res = await fetch(`/api/builds/${this.buildId}/logs?follow=1`, { signal: this.controller.signal });
      if (!res.ok || !res.body) {
        this.status = `unavailable (${res.status})`;
        return;
      }
      this.status = 'building';
      const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
      let carry = '';
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        carry += value;
        const parts = carry.split('\n');
        carry = parts.pop() ?? '';
        for (const raw of parts) this.take(raw);
      }
      if (carry) this.take(carry);
      if (this.status === 'building') this.status = 'ended';
    } catch (err) {
      if ((err as Error).name !== 'AbortError') this.status = 'disconnected';
    }
  }

  private take(raw: string) {
    if (!raw.trim()) return;
    let line: Line;
    try {
      line = JSON.parse(raw) as Line;
    } catch {
      this.appendText(raw);
      return;
    }
    if (line.line !== undefined) this.appendText(line.step ? `[${line.step}] ${line.line}` : line.line);
    if (line.error) {
      this.status = 'failed';
      this.failure = line.code ? `${line.error} (${line.code})` : line.error;
      return;
    }
    if (line.result) {
      this.status = 'built';
      this.appendText(`image ${line.result}`);
      if (this.autodeploy && !this.deployed) {
        this.deployed = true;
        void this.deploy(line.result);
      }
    }
  }

  private async deploy(build: string) {
    try {
      const res = await fetch(`/api/services/${this.serviceId}/deploy`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ build }),
      });
      if (!res.ok) {
        this.status = 'failed';
        this.failure = `The image was built but the deploy was refused (${res.status}).`;
        return;
      }
      navigate(`/services/${this.serviceId}?tab=deployments&ok=deployed` as Route);
    } catch (err) {
      this.status = 'failed';
      this.failure = (err as Error).message;
    }
  }

  private appendText(text: string) {
    const pane = this.pane.value;
    if (!pane) return;
    pane.appendChild(document.createTextNode(text + '\n'));
    this.lines += 1;
    while (this.lines > 2000 && pane.firstChild) {
      pane.removeChild(pane.firstChild);
      this.lines -= 1;
    }
    pane.scrollTop = pane.scrollHeight;
  }

  render() {
    const tone = this.status === 'built' ? 'default' : this.status === 'failed' ? 'destructive' : 'secondary';
    return html`
      <div class="mb-1.5 flex items-center justify-between gap-2">
        <span class=${badgeClass({ variant: tone })}>${this.status}</span>
        <a href=${`/api/builds/${this.buildId}/logs`} class="text-meta">Raw build log</a>
      </div>
      ${this.failure ? html`<p role="alert" class="m-0 mb-2 text-body text-destructive">${this.failure}</p>` : ''}
      <pre ${ref(this.pane)} class=${cn(PANE)} aria-live="polite" aria-label="Build output"></pre>
    `;
  }
}

BuildLog.register('build-log');
