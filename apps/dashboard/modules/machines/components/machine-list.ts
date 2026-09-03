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
      return html`<p class="text-muted-foreground">No machines yet. Create one with <code>pilot</code> or the API.</p>`;
    }
    return html`
      <div class="overflow-x-auto">
        <table class="w-full text-sm border-collapse">
          <thead>
            <tr class="text-left text-muted-foreground border-b border-border">
              <th class="py-2 pr-4 font-medium">Name</th>
              <th class="py-2 pr-4 font-medium">State</th>
              <th class="py-2 pr-4 font-medium">Host</th>
              <th class="py-2 pr-4 font-medium">URL</th>
              <th class="py-2 font-medium"><span class="sr-only">Actions</span></th>
            </tr>
          </thead>
          <tbody>
            ${this.rows.map(
              (m) => html`
                <tr class="border-b border-border">
                  <td class="py-2 pr-4">
                    <a href=${`/machines/${m.id}`} class="text-foreground">${m.name || m.id}</a>
                  </td>
                  <td class="py-2 pr-4"><span class="font-mono">${m.state}</span></td>
                  <td class="py-2 pr-4 text-muted-foreground">${m.host_id ?? ''}</td>
                  <td class="py-2 pr-4">
                    ${m.url ? html`<a href=${m.url} rel="noopener">${m.url}</a>` : ''}
                  </td>
                  <td class="py-2 text-right whitespace-nowrap">
                    <button
                      class="rounded-md border border-border px-2 py-1 hover:bg-muted disabled:opacity-50"
                      ?disabled=${this.busy === m.id}
                      @click=${() => this.act(m.id, m.state === 'suspended' ? 'wake' : 'suspend')}
                    >
                      ${m.state === 'suspended' ? 'Wake' : 'Suspend'}
                    </button>
                    <button
                      class="rounded-md border border-border px-2 py-1 hover:bg-muted disabled:opacity-50"
                      ?disabled=${this.busy === m.id}
                      @click=${() => this.act(m.id, 'destroy')}
                    >
                      Destroy
                    </button>
                  </td>
                </tr>
              `,
            )}
          </tbody>
        </table>
      </div>
      <p class="mt-3 text-xs text-muted-foreground">
        ${this.online ? 'Live' : 'Reconnecting'} · ${this.rows.length} machines
      </p>
    `;
  }
}

MachineList.register('machine-list');
