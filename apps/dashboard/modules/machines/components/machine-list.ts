/**
 * The live machine list.
 *
 * The rows arrive twice: once as SSR markup the page renders from its own
 * read, so the list is complete with JavaScript off, and then again over a
 * socket that pushes a snapshot and deltas. The component starts from the
 * server's rows via `.initial`, so hydration replaces the same list rather
 * than flashing empty.
 *
 * The socket is opened in `connectedCallback` and closed in
 * `disconnectedCallback`, so a client-router navigation away from this page
 * takes the subscription with it.
 *
 * This component owns its rows, which is why the chips and the filter live
 * INSIDE it rather than as a `<list-filter>` hiding rows underneath it: the
 * next socket delta would re-render the table and undo anything an outside
 * element had hidden.
 */

import { WebComponent, html, prop, connectWS, navigate, richFetch } from '@webjsdev/core';
import type { Route } from '@webjsdev/core';
import type { Machine } from '#modules/machines/types.ts';
import type { Host } from '#modules/fleet/types.ts';
import { buttonClass } from '#components/ui/button.ts';
import { badgeClass } from '#components/ui/badge.ts';
import { inputClass } from '#components/ui/input.ts';
import { kbdClass } from '#components/ui/kbd.ts';
import { skeletonClass } from '#components/ui/skeleton.ts';
import { toast } from '#components/ui/sonner.ts';
import { resumeTier } from '#modules/machines/utils/resume.ts';
import type { ResumeTier } from '#modules/machines/utils/resume.ts';
import { statusLine } from '#modules/machines/utils/ui/status-line.ts';
import { dataTable, emptyState } from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';
import '#components/relative-time.ts';
import '#components/copy-button.ts';
import '#components/ui/tooltip.ts';

interface Snapshot {
  type: 'snapshot';
  machines: Machine[];
}
interface Delta {
  type: 'delta';
  upsert: Machine[];
  remove: string[];
}

interface ServiceName {
  id: string;
  name: string;
}

/** Rows per page. Past this a list stops being readable and starts scrolling. */
const PAGE_SIZE = 100;

const CHIPS: { key: string; label: string }[] = [
  { key: 'all', label: 'All' },
  { key: 'running', label: 'running' },
  { key: 'warm', label: 'warm' },
  { key: 'cold', label: 'cold' },
  { key: 'other', label: 'other' },
];

class MachineList extends WebComponent({
  initial: prop<Machine[]>(Array),
  hosts: prop<Host[]>(Array),
  services: prop<ServiceName[]>(Array),
  /** Sandboxes only: no service grouping, and Open is the primary action. */
  sandboxes: prop(Boolean),
  rows: prop<Machine[]>(Array, { state: true }),
  online: prop(Boolean, { state: true }),
  busy: prop(String, { state: true }),
  chip: prop(String, { state: true }),
  query: prop(String, { state: true }),
  host: prop(String, { state: true }),
  page: prop(Number, { state: true }),
}) {
  private conn: { close(): void } | null = null;

  constructor() {
    super();
    this.initial = [];
    this.hosts = [];
    this.services = [];
    this.sandboxes = false;
    this.rows = [];
    this.online = false;
    this.busy = '';
    this.chip = 'all';
    this.query = '';
    this.host = '';
    this.page = 0;
  }

  willUpdate(changed: Map<string, unknown>) {
    // Seed from the server's rows the first time, so the SSR list and the
    // hydrated one are the same list.
    if (changed.has('initial') && this.rows.length === 0) this.rows = this.initial;
  }

  connectedCallback() {
    super.connectedCallback();
    this.conn = connectWS('/api/machines', {
      onOpen: () => {
        this.online = true;
      },
      onClose: () => {
        this.online = false;
      },
      onMessage: (message: Snapshot | Delta) => this.apply(message),
    });
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    this.conn?.close();
    this.conn = null;
  }

  private apply(message: Snapshot | Delta) {
    if (message?.type === 'snapshot') {
      this.rows = message.machines ?? [];
      return;
    }
    if (message?.type !== 'delta') return;

    const removed = new Set(message.remove ?? []);
    const next = this.rows.filter((m) => !removed.has(m.id));
    for (const row of message.upsert ?? []) {
      const at = next.findIndex((m) => m.id === row.id);
      if (at >= 0) next[at] = row;
      else next.push(row);
    }
    // A new array, not a mutation: the reactive property only re-renders on a
    // changed reference.
    this.rows = next;
  }

  private async act(id: string, action: 'suspend' | 'wake' | 'destroy') {
    this.busy = id;
    const name = this.rows.find((m) => m.id === id)?.name ?? id;
    try {
      const url = action === 'destroy' ? `/api/machines/${id}` : `/api/machines/${id}/${action}`;
      await richFetch(url, { method: action === 'destroy' ? 'DELETE' : 'POST' });
      // Past tense, because the engine has accepted it; the row's own state
      // still comes from the next socket tick and nothing is guessed here.
      toast.success(action === 'destroy' ? `Destroyed ${name}` : `${action === 'wake' ? 'Woke' : 'Suspended'} ${name}`);
    } catch (err) {
      // console.error was the only surface this had, which means the reader
      // saw a button that did nothing.
      toast.error(`Could not ${action} ${name}: ${err instanceof Error ? err.message : 'the request failed'}`);
    } finally {
      this.busy = '';
    }
  }

  /** The rows the chips, the filter and the host select leave visible. */
  private visible(): Machine[] {
    const needle = this.query.trim().toLowerCase();
    return this.rows.filter((m) => {
      if (this.chip !== 'all' && resumeTier(m, this.hosts) !== (this.chip as ResumeTier)) return false;
      if (this.host && m.host_id !== this.host) return false;
      if (needle) {
        const haystack = `${m.name ?? ''} ${m.id} ${m.state} ${m.host_id ?? ''} ${m.url ?? ''}`.toLowerCase();
        if (!haystack.includes(needle)) return false;
      }
      return true;
    });
  }

  private counts(): Record<string, number> {
    const out: Record<string, number> = { all: this.rows.length, running: 0, warm: 0, cold: 0, other: 0, boot: 0 };
    for (const m of this.rows) out[resumeTier(m, this.hosts)] = (out[resumeTier(m, this.hosts)] ?? 0) + 1;
    // `boot` is a resume tier but not a chip: a stopped machine is rare and
    // reads as "other" to anyone who is not thinking about the ladder.
    out.other = (out.other ?? 0) + (out.boot ?? 0);
    return out;
  }

  private serviceName(id: string | undefined): string {
    if (!id) return 'Sandboxes';
    return this.services.find((s) => s.id === id)?.name ?? id;
  }

  render() {
    if (this.rows.length === 0) {
      // An empty list on a page that rendered while the fleet was unreachable
      // is not the same as an org with no machines, but neither the socket nor
      // the SSR read can tell us which, so the skeleton only shows while the
      // socket has not connected yet.
      if (!this.online && this.initial.length === 0) {
        return html`<div class="grid gap-2" aria-busy="true" aria-label="Loading machines">
          ${[0, 1, 2].map(() => html`<div class=${cn(skeletonClass(), 'h-10 w-full')}></div>`)}
        </div>`;
      }
      return emptyState(
        this.sandboxes ? 'No sandboxes yet.' : 'No machines yet.',
        { command: 'pilot machines create' },
      );
    }

    const rows = this.visible();
    const counts = this.counts();
    const pages = Math.max(1, Math.ceil(rows.length / PAGE_SIZE));
    const page = Math.min(this.page, pages - 1);
    const paged = rows.slice(page * PAGE_SIZE, (page + 1) * PAGE_SIZE);
    const hosts = [...new Set(this.rows.map((m) => m.host_id).filter(Boolean))] as string[];

    return html`
      ${this.toolbar(counts, hosts)}
      ${rows.length === 0
        ? emptyState('Nothing matches that filter.')
        : html`<div @click=${this.rowClick}>
            ${dataTable<Machine>({
              caption: this.sandboxes ? 'Sandboxes in this organisation' : 'Machines in this organisation',
              rows: paged,
              rowHref: (m) => `/machines/${m.id}`,
              columns: this.columns(),
            })}
          </div>`}
      ${pages > 1 ? this.pager(page, pages) : ''}
      <p class="mt-3 flex items-center gap-2 text-xs text-muted-foreground">
        <ui-tooltip>
          <ui-tooltip-trigger>
            <span
              tabindex="0"
              aria-label=${this.online ? 'Live' : 'Reconnecting'}
              class=${cn('inline-block size-1.5 rounded-full align-middle', this.online ? 'bg-primary' : 'bg-muted-foreground')}
            ></span>
          </ui-tooltip-trigger>
          <ui-tooltip-content side="top"
            >${this.online ? 'Live: this list updates as machines change.' : 'Reconnecting to the live feed.'}</ui-tooltip-content
          >
        </ui-tooltip>
        ${rows.length === this.rows.length
          ? html`${this.rows.length} machines`
          : html`${rows.length} of ${this.rows.length} machines`}
      </p>
    `;
  }

  private toolbar(counts: Record<string, number>, hosts: string[]) {
    return html`
      <div class="mb-3 flex flex-wrap items-center gap-2">
        ${CHIPS.map((chip) => {
          const on = this.chip === chip.key;
          return html`<button
            type="button"
            aria-pressed=${on ? 'true' : 'false'}
            class=${cn(badgeClass({ variant: on ? 'default' : 'outline' }), 'cursor-pointer')}
            @click=${() => {
              this.chip = chip.key;
              this.page = 0;
            }}
          >
            ${chip.label} ${counts[chip.key] ?? 0}
          </button>`;
        })}

        <label class="sr-only" for="machine-filter">Filter machines</label>
        <input
          id="machine-filter"
          type="search"
          placeholder="Filter machines"
          .value=${this.query}
          class=${cn(inputClass(), 'ml-auto h-8 w-56')}
          @input=${(e: Event) => {
            this.query = (e.target as HTMLInputElement).value;
            this.page = 0;
          }}
        >
        <kbd class=${kbdClass()} aria-hidden="true">/</kbd>

        ${hosts.length > 1
          ? html`<label class="sr-only" for="machine-host">Host</label>
              <select
                id="machine-host"
                class=${cn(inputClass(), 'h-8 w-40')}
                @change=${(e: Event) => {
                  this.host = (e.target as HTMLSelectElement).value;
                  this.page = 0;
                }}
              >
                <option value="">Every host</option>
                ${hosts.map((h) => html`<option value=${h} ?selected=${this.host === h}>${h}</option>`)}
              </select>`
          : ''}
      </div>
    `;
  }

  private pager(page: number, pages: number) {
    return html`
      <div class="mt-3 flex items-center gap-3 text-sm">
        <button
          type="button"
          class=${buttonClass({ variant: 'outline', size: 'sm' })}
          ?disabled=${page === 0}
          @click=${() => (this.page = Math.max(0, page - 1))}
        >
          Previous
        </button>
        <span class="text-muted-foreground">Page ${page + 1} of ${pages}</span>
        <button
          type="button"
          class=${buttonClass({ variant: 'outline', size: 'sm' })}
          ?disabled=${page >= pages - 1}
          @click=${() => (this.page = Math.min(pages - 1, page + 1))}
        >
          Next
        </button>
      </div>
    `;
  }

  /**
   * Whole rows navigate.
   *
   * Handled here rather than by wrapping the table in `<link-rows>`, because
   * this component re-renders its own table on every socket delta: a wrapper
   * projecting rows it did not render would have to re-adopt them each time.
   * `<link-rows>` is for the tables a PAGE renders once.
   */
  private rowClick = (event: MouseEvent) => {
    if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) {
      return;
    }
    const target = event.target as HTMLElement | null;
    if (!target || target.closest('a, button, input, select, textarea, label')) return;
    if ((globalThis.getSelection?.()?.toString() ?? '').length > 0) return;
    const href = target.closest<HTMLElement>('[data-href]')?.dataset.href;
    if (!href) return;
    event.preventDefault();
    navigate(href as Route);
  };

  private columns() {
    const stop = (event: Event) => event.stopPropagation();
    return [
      {
        header: 'Name',
        cell: (m: Machine) => html`
          <a href=${`/machines/${m.id}`} class="text-foreground">${m.name || m.id}</a>
          ${this.sandboxes || !m.service_id
            ? ''
            : html`<span class="block text-xs text-muted-foreground">${this.serviceName(m.service_id)}</span>`}
        `,
      },
      {
        header: 'Status',
        cell: (m: Machine) =>
          this.busy === m.id
            ? html`<span class=${cn(skeletonClass(), 'inline-block h-5 w-40 align-middle')} aria-busy="true"></span>`
            : statusLine(m, this.hosts),
      },
      { header: 'Host', cellClass: 'font-mono text-muted-foreground', cell: (m: Machine) => m.host_id ?? '' },
      {
        header: 'URL',
        cell: (m: Machine) =>
          m.url
            ? html`<span class="flex items-center gap-1">
                <a href=${m.url} rel="noopener" @click=${stop}>${m.url}</a>
                <copy-button value=${m.url} label="URL"></copy-button>
                ${m.state === 'running' ? '' : html`<span class="text-xs text-muted-foreground">wakes on request</span>`}
              </span>`
            : '',
      },
      {
        header: 'Actions',
        headerHidden: true,
        align: 'right' as const,
        cellClass: 'whitespace-nowrap',
        // Icons with tooltips for the two navigations, words for the two
        // actions that change something. Four labelled buttons per row pushed
        // the table wider than the page and clipped the last one, and the two
        // that go somewhere are the ones a reader recognises by shape.
        cell: (m: Machine) => html`
          <ui-tooltip>
            <ui-tooltip-trigger>
              <a
                href=${`/machines/${m.id}/terminal`}
                class=${buttonClass({ variant: this.sandboxes ? 'default' : 'ghost', size: 'icon-sm' })}
                aria-label=${`Open a terminal on ${m.name || m.id}`}
                @click=${stop}
              >
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m4 17 6-6-6-6M12 19h8" /></svg>
              </a>
            </ui-tooltip-trigger>
            <ui-tooltip-content side="top">Open a terminal</ui-tooltip-content>
          </ui-tooltip>

          <ui-tooltip>
            <ui-tooltip-trigger>
              <a
                href=${`/machines/${m.id}#console`}
                class=${buttonClass({ variant: 'ghost', size: 'icon-sm' })}
                aria-label=${`Logs for ${m.name || m.id}`}
                @click=${stop}
              >
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M15 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V7Z" /><path d="M14 2v5h5M8 13h8M8 17h5" /></svg>
              </a>
            </ui-tooltip-trigger>
            <ui-tooltip-content side="top">Logs</ui-tooltip-content>
          </ui-tooltip>

          <button
            class=${buttonClass({ variant: 'outline', size: 'xs' })}
            ?disabled=${this.busy === m.id}
            @click=${(e: Event) => {
              stop(e);
              void this.act(m.id, m.state === 'suspended' ? 'wake' : 'suspend');
            }}
          >
            ${m.state === 'suspended' ? 'Wake' : 'Suspend'}
          </button>

          <ui-tooltip>
            <ui-tooltip-trigger>
              <button
                class=${cn(buttonClass({ variant: 'ghost', size: 'icon-sm' }), 'text-muted-foreground hover:text-destructive')}
                aria-label=${`Destroy ${m.name || m.id}`}
                ?disabled=${this.busy === m.id}
                @click=${(e: Event) => {
                  stop(e);
                  // A window.confirm until the alert dialog lands. It is
                  // deliberately not nothing: destroy is irreversible and the
                  // button sits one row away from Suspend.
                  if (globalThis.confirm?.(`Destroy ${m.name || m.id}? This cannot be undone.`)) {
                    void this.act(m.id, 'destroy');
                  }
                }}
              >
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 6h18M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2m3 0v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6" /></svg>
              </button>
            </ui-tooltip-trigger>
            <ui-tooltip-content side="top">Destroy, permanently</ui-tooltip-content>
          </ui-tooltip>
        `,
      },
    ];
  }
}

MachineList.register('machine-list');
