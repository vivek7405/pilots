/**
 * <log-stream>: every running instance's console, tailed into one table.
 *
 * One `fetch('/api/machines/<id>/logs')` per source, appended imperatively into
 * a `<tbody>` that is rendered ONCE. A component that re-rendered its rows on
 * every line would wipe what it had already streamed, so the rows are DOM the
 * framework preserves across a toolbar re-render (the `ref`'d tbody survives,
 * the way `log-pane` relied on before this replaced it), and `render()` never
 * reads the row data.
 *
 * The toolbar (a filter box, service and instance chips, a follow toggle) is
 * reactive, so its state lives in props; changing one re-runs `applyFilter`
 * over the rows already in the tbody rather than rebuilding them.
 *
 * hostd's lines carry no timestamp, so the time column is when the line reached
 * this page. Output starts at each instance's last boot, which is all pilots
 * keeps, and the footnote says so.
 */
import { WebComponent, html, prop } from '@webjsdev/core';
import { createRef, ref } from '@webjsdev/core/directives';
import { badgeClass } from '#components/ui/badge.ts';
import { inputClass } from '#components/ui/input.ts';
import { footnote } from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';

export interface LogSource {
  /** The machine id the log route is keyed by. */
  id: string;
  /** The service this instance serves, or "Sandbox" for a loose machine. */
  service: string;
  /** The instance's own name. */
  name: string;
}

/** At most this many streams open at once; a busier org filters first. */
const MAX_SOURCES = 25;
/** Trim the table past this many rows so a chatty fleet cannot fill a tab. */
const MAX_ROWS = 5000;

export class LogStream extends WebComponent({
  sources: prop<LogSource[]>(Array),
  query: prop(String, { state: true }),
  service: prop(String, { state: true }),
  instance: prop(String, { state: true }),
  follow: prop(Boolean, { state: true }),
  live: prop(Number, { state: true }),
}) {
  private body = createRef<HTMLTableSectionElement>();
  private controllers: AbortController[] = [];
  private rows = 0;

  constructor() {
    super();
    this.sources = [];
    this.query = '';
    this.service = '';
    this.instance = '';
    this.follow = true;
    this.live = 0;
  }

  connectedCallback() {
    super.connectedCallback();
    for (const source of this.sources.slice(0, MAX_SOURCES)) this.open(source);
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    for (const c of this.controllers) c.abort();
    this.controllers = [];
  }

  private async open(source: LogSource) {
    const controller = new AbortController();
    this.controllers.push(controller);
    try {
      const res = await fetch(`/api/machines/${source.id}/logs`, { signal: controller.signal });
      if (!res.ok || !res.body) return;
      this.live += 1;
      const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
      let carry = '';
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        carry += value;
        const parts = carry.split('\n');
        carry = parts.pop() ?? '';
        for (const line of parts) this.appendRow(source, line);
      }
      if (carry) this.appendRow(source, carry);
    } catch (err) {
      if ((err as Error).name !== 'AbortError') this.appendRow(source, `[log stream ended: ${(err as Error).message}]`);
    } finally {
      this.live = Math.max(0, this.live - 1);
    }
  }

  private appendRow(source: LogSource, text: string) {
    const body = this.body.value;
    if (!body) return;
    const tr = document.createElement('tr');
    tr.className = 'border-t border-border align-top';
    tr.dataset.service = source.service;
    tr.dataset.instance = source.name;
    tr.dataset.text = text;

    const time = document.createElement('td');
    time.className = 'whitespace-nowrap py-1 pr-3 text-muted-foreground tabular-nums';
    time.textContent = new Date().toLocaleTimeString();
    const svc = document.createElement('td');
    svc.className = 'whitespace-nowrap py-1 pr-3';
    svc.textContent = source.service;
    const inst = document.createElement('td');
    inst.className = 'whitespace-nowrap py-1 pr-3 text-muted-foreground';
    inst.textContent = source.name;
    const msg = document.createElement('td');
    msg.className = 'py-1 whitespace-pre-wrap break-all';
    // textContent, never innerHTML: a guest writes these bytes.
    msg.textContent = text;

    tr.append(time, svc, inst, msg);
    tr.hidden = !this.rowVisible(tr);
    body.appendChild(tr);
    this.rows += 1;
    while (this.rows > MAX_ROWS && body.firstChild) {
      body.removeChild(body.firstChild);
      this.rows -= 1;
    }
    if (this.follow) {
      const scroller = body.closest('[data-log-scroll]');
      if (scroller) scroller.scrollTop = scroller.scrollHeight;
    }
  }

  private rowVisible(tr: HTMLTableRowElement): boolean {
    if (this.service && tr.dataset.service !== this.service) return false;
    if (this.instance && tr.dataset.instance !== this.instance) return false;
    if (this.query && !(tr.dataset.text ?? '').toLowerCase().includes(this.query.toLowerCase())) return false;
    return true;
  }

  private applyFilter() {
    const body = this.body.value;
    if (!body) return;
    for (const el of Array.from(body.children)) {
      const tr = el as HTMLTableRowElement;
      tr.hidden = !this.rowVisible(tr);
    }
  }

  private onQuery = (e: Event) => {
    this.query = (e.target as HTMLInputElement).value;
    this.applyFilter();
  };
  private pickService = (value: string) => {
    this.service = this.service === value ? '' : value;
    this.applyFilter();
  };
  private pickInstance = (value: string) => {
    this.instance = this.instance === value ? '' : value;
    this.applyFilter();
  };
  private toggleFollow = () => {
    this.follow = !this.follow;
  };

  render() {
    const services = [...new Set(this.sources.map((s) => s.service))];
    const instances = [...new Set(this.sources.map((s) => s.name))];
    const chip = (label: string, on: boolean, onClick: () => void) => html`<button
      type="button"
      aria-pressed=${on ? 'true' : 'false'}
      @click=${onClick}
      class=${cn(
        'rounded-full border px-2.5 py-1 text-meta transition-colors',
        on ? 'border-primary bg-accent font-medium text-foreground' : 'border-border text-muted-foreground hover:text-foreground',
      )}
    >
      ${label}
    </button>`;

    return html`
      <div class="flex flex-col gap-3">
        <div class="flex flex-wrap items-center gap-2">
          <input
            type="search"
            placeholder="Filter lines"
            .value=${this.query}
            @input=${this.onQuery}
            class=${cn(inputClass(), 'max-w-56')}
            aria-label="Filter log lines"
          />
          <button
            type="button"
            aria-pressed=${this.follow ? 'true' : 'false'}
            @click=${this.toggleFollow}
            class=${cn(
              'rounded-md border px-2.5 py-1 text-meta transition-colors',
              this.follow ? 'border-primary bg-accent font-medium text-foreground' : 'border-border text-muted-foreground hover:text-foreground',
            )}
          >
            ${this.follow ? 'Following' : 'Follow'}
          </button>
          <span class=${badgeClass({ variant: this.live > 0 ? 'secondary' : 'outline' })}
            >${this.live} live</span
          >
        </div>

        ${services.length > 1
          ? html`<div class="flex flex-wrap items-center gap-1.5">
              <span class="text-meta text-muted-foreground">Service</span>
              ${services.map((s) => chip(s, this.service === s, () => this.pickService(s)))}
            </div>`
          : ''}
        ${instances.length > 1
          ? html`<div class="flex flex-wrap items-center gap-1.5">
              <span class="text-meta text-muted-foreground">Instance</span>
              ${instances.map((n) => chip(n, this.instance === n, () => this.pickInstance(n)))}
            </div>`
          : ''}

        <div data-log-scroll class="h-[28rem] overflow-auto rounded-md border border-border bg-muted">
          <table class="w-full border-collapse px-3 text-meta font-mono">
              <caption class="sr-only">Live log output from your running instances</caption>
            <thead class="sticky top-0 bg-muted">
              <tr class="text-left text-muted-foreground">
                <th scope="col" class="py-1.5 pl-3 pr-3 font-medium">Received</th>
                <th scope="col" class="py-1.5 pr-3 font-medium">Service</th>
                <th scope="col" class="py-1.5 pr-3 font-medium">Instance</th>
                <th scope="col" class="py-1.5 pr-3 font-medium">Message</th>
              </tr>
            </thead>
            <tbody ${ref(this.body)} class="[&_td:first-child]:pl-3"></tbody>
          </table>
        </div>
        ${footnote(
          'Times are when a line reached this page. Output starts at each instance’s last boot, which is all pilots keeps.',
        )}
      </div>
    `;
  }
}

LogStream.register('log-stream');
