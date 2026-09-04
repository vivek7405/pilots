/**
 * The fleet's hosts, live.
 *
 * This is the one feed the framework's `broadcast` is right for: liveness and
 * free capacity belong to no tenant, so every signed-in viewer is entitled to
 * the same message. Every per-org feed keeps its own subscriber set instead.
 */

import { WebComponent, html, prop, connectWS } from '@webjsdev/core';
import { badgeClass } from '#components/ui/badge.ts';
import { cardClass } from '#components/ui/card.ts';
import { emptyState } from '#lib/utils/ui.ts';
import { cn } from '#lib/utils/cn.ts';

interface Host {
  id: string;
  alive: boolean;
  cpu_free: number;
  mem_free_mib: number;
}

class HostsStrip extends WebComponent({
  initial: prop<Host[]>(Array),
  hosts: prop<Host[]>(Array, { state: true }),
}) {
  private conn: { close(): void } | null = null;

  constructor() {
    super();
    this.initial = [];
    this.hosts = [];
  }

  willUpdate(changed: Map<string, unknown>) {
    if (changed.has('initial') && this.hosts.length === 0) this.hosts = this.initial;
  }

  connectedCallback() {
    super.connectedCallback();
    this.conn = connectWS('/api/hosts', {
      onMessage: (message: { hosts?: Host[] }) => {
        if (Array.isArray(message?.hosts)) this.hosts = message.hosts;
      },
    });
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    this.conn?.close();
    this.conn = null;
  }

  render() {
    if (this.hosts.length === 0) return emptyState('No hosts reporting.');
    return html`
      <ul class="flex flex-wrap gap-3 list-none p-0 m-0 text-sm">
        ${this.hosts.map(
          (h) => html`
            <li class=${cn(cardClass({ size: 'sm' }), 'flex-row items-center gap-3 px-4')} data-slot="card" data-size="sm">
              <span class="font-mono">${h.id}</span>
              <span
                class=${h.alive
                  ? badgeClass({ variant: 'secondary' })
                  : cn(badgeClass({ variant: 'outline' }), 'border-destructive/40 text-destructive')}
              >
                ${h.alive ? 'up' : 'down'}
              </span>
              <span class="text-muted-foreground tabular-nums">${h.cpu_free} cpu · ${h.mem_free_mib} MiB</span>
            </li>
          `,
        )}
      </ul>
    `;
  }
}

HostsStrip.register('hosts-strip');
