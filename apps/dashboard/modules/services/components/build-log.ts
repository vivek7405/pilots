/**
 * <build-log>: a build's output as it happens, and the deployment it became.
 *
 * Rendered ONCE and appended to imperatively, like the log stream: a
 * component that re-rendered on every line would wipe the lines it had. It
 * reads `/api/builds/<id>/logs?follow=1` as NDJSON, appends `step` and `line`,
 * and keeps the last two thousand.
 *
 * It READS the verdict; it does not decide it. The build was started with the
 * deploy it is for (`?deploy=<service>`), so the host cuts the release on the
 * verdict and puts its id on the log's last line. This element sees `release`
 * and navigates to it. That is the whole point of the shape: a tab that is
 * closed, a laptop lid that shuts, or a second tab open on the same build
 * changes nothing about whether a release is cut, or how many.
 *
 * A terminal `error` line is an alert carrying the engine's own words --
 * `error`, `code` and `next` -- whether it failed in the build or in the
 * health gate afterwards. With scripting off the page shows the raw log link
 * and the Deploy form takes the image id by hand, so the verdict is never
 * lost.
 */
import { WebComponent, html, prop, navigate } from '@webjsdev/core';
import { createRef, ref } from '@webjsdev/core/directives';
import type { Route } from '@webjsdev/core';
import { badgeClass } from '#components/ui/badge.ts';
import { cn } from '#lib/utils/cn.ts';

const PANE = 'h-72 overflow-auto rounded-md border border-border bg-muted p-3 text-meta font-mono whitespace-pre-wrap';

export interface Line {
  step?: string;
  line?: string;
  error?: string;
  code?: string;
  result?: string;
  release?: string;
  next?: string;
}

/**
 * What a terminal line says happened, or null while the build is still going.
 *
 * A function of the line alone, exported so the reading of the engine's
 * verdict is testable without a browser. The four outcomes are the four the
 * log can end on: the image exists, the release exists, something refused, or
 * nothing terminal yet.
 */
export function verdictOf(line: Line): { kind: 'built' | 'deployed' | 'failed'; text: string } | null {
  // A failure FIRST: a refused deploy carries `next` and no `release`, and a
  // failed build carries neither. Either way the reason outranks the image.
  if (line.error) return { kind: 'failed', text: failureText(line) };
  if (line.release) return { kind: 'deployed', text: line.release };
  if (line.result) return { kind: 'built', text: line.result };
  return null;
}

/**
 * The engine's own words for a failure, not a bare status.
 *
 * A build that did not compile and a health gate that never passed both land
 * here. The second names the instance whose console says why and what to do
 * about it, and dropping that into "refused (422)" was the one path where a
 * deploy's verdict never reached the person who could act on it.
 */
export function failureText(line: Line): string {
  return [line.error ?? '', line.code ? `(${line.code})` : '', line.next ?? '']
    .filter(Boolean)
    .join(' ');
}

export class BuildLog extends WebComponent({
  buildId: prop(String, { attribute: 'build-id' }),
  serviceId: prop(String, { attribute: 'service-id' }),
  /** This build was started with a deploy attached, so a release is coming. */
  autodeploy: prop(Boolean),
  /** Where a finished deploy lands. Empty means the service page. */
  back: prop(String),
  status: prop(String, { state: true }),
  failure: prop(String, { state: true }),
}) {
  private pane = createRef<HTMLPreElement>();
  private controller: AbortController | null = null;
  private lines = 0;
  private built = '';
  private left = false;

  constructor() {
    super();
    this.buildId = '';
    this.serviceId = '';
    this.back = '';
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
      // A stream that ended after the image and before any release. Usually a
      // build started by a version of this app that deployed from the browser,
      // so no release is coming -- but the same thing is seen when the host
      // restarted mid-rollout, or when the log ended for its own reasons while
      // the rollout carried on. What is observed is only that this connection
      // ended first, so that is all this says: claiming "no deploy" and
      // pointing at the Deploy form would invite a SECOND release for a
      // rollout that may still be running, which is the thing this shape
      // exists to make impossible.
      if (this.autodeploy && this.built && this.status === 'deploying') {
        this.status = 'built';
        this.failure =
          `The image ${this.built} was built, but this connection ended before a release did. ` +
          'Check the deployments below; if none was cut, deploy the image with the form.';
      }
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
    const verdict = verdictOf(line);
    if (!verdict) return;
    if (verdict.kind === 'failed') {
      this.status = 'failed';
      this.failure = verdict.text;
      return;
    }
    if (verdict.kind === 'built' && !this.built) {
      this.built = verdict.text;
      this.status = this.autodeploy ? 'deploying' : 'built';
      this.appendText(`image ${verdict.text}`);
      return;
    }
    // The release the HOST cut, on the log's last line. Nothing is posted from
    // here: this element reads a verdict that has already happened, so a
    // second tab following the same build lands on the same deployment rather
    // than rolling it out again.
    if (verdict.kind === 'deployed') {
      this.status = 'deployed';
      this.land();
    }
  }

  /**
   * Back to where the build was followed from -- the canvas slide-over when it
   * started there -- rather than always the service page.
   *
   * Guarded, because the release line is in the RECORDED log: a reader who
   * arrives after the deploy replays it, and every follower of one build runs
   * this. One navigation per element.
   */
  private land() {
    if (this.left) return;
    this.left = true;
    const target = this.back || `/services/${this.serviceId}?tab=deployments`;
    navigate(`${target}${target.includes('?') ? '&' : '?'}ok=deployed` as Route);
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
