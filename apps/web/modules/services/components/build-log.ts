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
 *
 * THE LOG IS NOT THE ONLY WITNESS, and it must not be. A build's log lives on
 * the host that ran it: hostd answers `GET /v1/builds/{id}/logs` from its own
 * memory and 404s for a build it does not have, while this app reaches the
 * fleet at a hostname every host answers, so on a fleet of more than one the
 * log is a coin toss. The RELEASE is a replicated row, readable from any host,
 * so whenever the log cannot deliver a verdict -- it was not there, the
 * connection ended early, the host restarted -- the element watches
 * `GET /api/services/<id>` for a release that is not the one the page rendered
 * with. Nothing here ever POSTs a deploy: a second rollout is precisely what
 * the server-side verdict exists to make impossible.
 */
import { WebComponent, html, prop, navigate } from '@webjsdev/core';
import { createRef, ref } from '@webjsdev/core/directives';
import type { Route } from '@webjsdev/core';
import { badgeClass } from '#components/ui/badge.ts';
import { cn } from '#lib/utils/cn.ts';

const PANE = 'h-72 overflow-auto rounded-md border border-border bg-muted p-3 text-meta font-mono whitespace-pre-wrap';

/**
 * How the release row is watched when the log cannot deliver the verdict.
 *
 * The ceiling is a rollout's own worst case, not a guess: a deploy boots a
 * replica, gates it for the health check's grace -- routinely minutes -- and
 * restores the rest from its snapshot. Giving up sooner would report "no
 * deployment" for one that is simply still gating.
 */
const RELEASE_POLL_MS = 3_000;
const WAIT_FOR_RELEASE_MS = 10 * 60_000;

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
  /**
   * A release from this build is still to come, so the element waits for it
   * and moves the reader onto it. False for a build older than the deployment
   * running now: its log replays a `release` line that was acted on long ago,
   * and acting on it again would bounce the reader out of the log they asked
   * to read.
   */
  autodeploy: prop(Boolean),
  /** Where a finished deploy lands. Empty means the service page. */
  back: prop(String),
  /**
   * The deployment the page rendered with. Anything else is new, and nothing
   * equal to it is: it is already on the screen behind this log.
   */
  releaseId: prop(String, { attribute: 'release-id' }),
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
    this.releaseId = '';
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
        // A build's log lives on the host that ran it, and this request lands
        // on whichever host answered. A 404 here says "not on this host", not
        // "your deploy failed" -- and a build that carries a deploy runs on
        // the service's arbiter, which is a host the browser never chose. So
        // the release is watched for where it actually is: a replicated row.
        this.status = this.autodeploy ? 'deploying' : `unavailable (${res.status})`;
        if (!this.autodeploy) return;
        this.appendText(`this build's log is on another host; watching for the deployment instead`);
        await this.awaitRelease();
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
      // A stream that ended after the image and before any release. What is
      // observed is only that this connection ended first: the rollout may
      // still be running, the host may have restarted, the build may predate
      // the deploy travelling with it. So the release row is asked, rather
      // than any of that being claimed -- and never the Deploy form, which
      // would invite a SECOND release for a rollout still in flight.
      if (this.autodeploy && this.status === 'deploying') await this.awaitRelease();
    } catch (err) {
      if ((err as Error).name !== 'AbortError') this.status = 'disconnected';
    }
  }

  /**
   * The deployment, from the replicated row when the log could not say.
   *
   * The service's release id is readable from any host, which is the point:
   * the log is held by one host and this app talks to whichever answers. A
   * release that is not the one the page rendered with is this build's, and
   * the reader is moved onto it exactly as a `release` line would have.
   *
   * It only ever READS. A poll that gave up and posted a deploy would be the
   * double rollout this whole shape removes, arrived at from the other side.
   */
  private async awaitRelease() {
    const deadline = Date.now() + WAIT_FOR_RELEASE_MS;
    while (Date.now() < deadline && !this.left) {
      await new Promise((r) => setTimeout(r, RELEASE_POLL_MS));
      if (this.controller?.signal.aborted) return;
      try {
        const res = await fetch(`/api/services/${this.serviceId}`, { signal: this.controller?.signal });
        if (!res.ok) continue;
        const service = (await res.json()) as { release_id?: string };
        if (service.release_id && service.release_id !== this.releaseId) {
          this.status = 'deployed';
          this.land();
          return;
        }
      } catch (err) {
        if ((err as Error).name === 'AbortError') return;
      }
    }
    if (this.left) return;
    this.status = 'built';
    this.failure =
      'The image was built, and no deployment has appeared yet. ' +
      'The deployments below are the record; reload to see one that arrives later.';
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
      // Only a release the page does not already show. A log replays, so this
      // line arrives again on every reload of a finished build -- and moving
      // the reader for a deployment that is already on the screen behind them
      // announces news that is not news. `autodeploy` is the other half: a
      // build older than the running deployment is a log to read, not a
      // deploy to follow.
      if (this.autodeploy && verdict.text !== this.releaseId) this.land();
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
