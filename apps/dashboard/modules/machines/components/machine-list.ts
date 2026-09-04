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
 */

import { WebComponent, html, prop, connectWS, richFetch } from '@webjsdev/core';
import type { Machine } from '#modules/machines/types.ts';
import { buttonClass } from '#components/ui/button.ts';
import { badgeClass } from '#components/ui/badge.ts';
import { stateBadge } from '#modules/machines/utils/ui/state.ts';
import { dataTable, emptyState } from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';

interface Snapshot {
  type: 'snapshot';
  machines: Machine[];
}
interface Delta {
  type: 'delta';
  upsert: Machine[];
  remove: string[];
}

class MachineList extends WebComponent({
  initial: prop<Machine[]>(Array),
  rows: prop<Machine[]>(Array, { state: true }),
  online: prop(Boolean, { state: true }),
  busy: prop(String, { state: true }),
}) {
  private conn: { close(): void } | null = null;

  constructor() {
    super();
    this.initial = [];
    this.rows = [];
    this.online = false;
    this.busy = '';
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
    try {
      const url = action === 'destroy' ? `/api/machines/${id}` : `/api/machines/${id}/${action}`;
      await richFetch(url, { method: action === 'destroy' ? 'DELETE' : 'POST' });
    } catch (err) {
      console.error(`machine ${action} failed`, err);
    } finally {
      this.busy = '';
      // The next tick carries the real state; nothing is guessed locally.
    }
  }

  render() {
    if (this.rows.length === 0) {
      return emptyState(html`No machines yet. Create one with <code class="font-mono">pilot</code> or the API.`);
    }
    return html`
      ${dataTable<Machine>({
        caption: 'Machines in this organisation',
        rows: this.rows,
        columns: [
          {
            header: 'Name',
            cell: (m) => html`<a href=${`/machines/${m.id}`} class="text-foreground">${m.name || m.id}</a>`,
          },
          { header: 'State', cell: (m) => stateBadge(m.state) },
          { header: 'Host', cellClass: 'font-mono text-muted-foreground', cell: (m) => m.host_id ?? '' },
          { header: 'URL', cell: (m) => (m.url ? html`<a href=${m.url} rel="noopener">${m.url}</a>` : '') },
          {
            header: 'Actions',
            headerHidden: true,
            align: 'right',
            cellClass: 'whitespace-nowrap',
            cell: (m) => html`
              <button
                class=${buttonClass({ variant: 'outline', size: 'xs' })}
                ?disabled=${this.busy === m.id}
                @click=${() => this.act(m.id, m.state === 'suspended' ? 'wake' : 'suspend')}
              >
                ${m.state === 'suspended' ? 'Wake' : 'Suspend'}
              </button>
              <button
                class=${cn(buttonClass({ variant: 'ghost', size: 'xs' }), 'text-muted-foreground hover:text-destructive')}
                ?disabled=${this.busy === m.id}
                @click=${() => this.act(m.id, 'destroy')}
              >
                Destroy
              </button>
            `,
          },
        ],
      })}
      <p class="mt-3 flex items-center gap-2 text-xs text-muted-foreground">
        <span class=${badgeClass({ variant: this.online ? 'secondary' : 'outline' })}>
          ${this.online ? 'Live' : 'Reconnecting'}
        </span>
        ${this.rows.length} machines
      </p>
    `;
  }
}

MachineList.register('machine-list');
