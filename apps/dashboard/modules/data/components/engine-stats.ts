/**
 * The engine's own numbers, fetched when the tab is on screen.
 *
 * Fetched rather than server-rendered because reading them runs a command
 * inside the machine, and a page that waited for that would be a page whose
 * whole load time is a database's response time. The tab paints, then fills.
 */
import { WebComponent, html, prop } from '@webjsdev/core';
import type { TemplateResult } from '@webjsdev/core';

interface Stats {
  connections?: number;
  max_connections?: number;
  cache_hit_ratio?: number;
  commits?: number;
  rollbacks?: number;
  data_bytes?: number;
  uptime_seconds?: number;
  error?: string;
}

export class EngineStats extends WebComponent({
  serviceId: String,
  loading: Boolean,
  stats: prop<Stats | null>(Object),
}) {
  constructor() {
    super();
    this.serviceId = '';
    this.loading = true;
    this.stats = null;
  }

  connectedCallback(): void {
    super.connectedCallback?.();
    void this.load();
  }

  private async load(): Promise<void> {
    this.loading = true;
    try {
      const res = await fetch(`/api/services/${encodeURIComponent(this.serviceId)}/engine`);
      this.stats = (await res.json()) as Stats;
    } catch (err) {
      this.stats = { error: err instanceof Error ? err.message : 'could not reach the engine' };
    } finally {
      this.loading = false;
    }
  }

  /**
   * Connections against the limit, with the warning as part of the number.
   *
   * "42" means nothing. "42 / 100" means something. "95 / 100" means do
   * something now, which is why the sentence is attached to the figure rather
   * than left for a reader to work out.
   */
  private connections(stats: Stats): TemplateResult {
    const used = stats.connections ?? 0;
    const limit = stats.max_connections ?? 0;
    if (limit <= 0) return html`${used}`;
    const near = used >= 0.8 * limit;
    return html`${used} / ${limit}
      ${near
        ? html`<span class="text-warning">Near the limit. A pooler is what fixes this.</span>`
        : ''}`;
  }

  private rows(stats: Stats): { label: string; value: TemplateResult | string }[] {
    const out: { label: string; value: TemplateResult | string }[] = [
      { label: 'Connections', value: this.connections(stats) },
    ];
    if ((stats.cache_hit_ratio ?? 0) > 0) {
      out.push({ label: 'Served from memory', value: `${((stats.cache_hit_ratio ?? 0) * 100).toFixed(2)}%` });
    }
    if ((stats.commits ?? 0) > 0 || (stats.rollbacks ?? 0) > 0) {
      out.push({ label: 'Commits', value: String(stats.commits ?? 0) });
      out.push({ label: 'Rollbacks', value: String(stats.rollbacks ?? 0) });
    }
    if ((stats.data_bytes ?? 0) > 0) {
      out.push({ label: 'Data', value: humanBytes(stats.data_bytes ?? 0) });
    }
    if ((stats.uptime_seconds ?? 0) > 0) {
      out.push({ label: 'Engine uptime', value: humanDuration(stats.uptime_seconds ?? 0) });
    }
    return out;
  }

  render(): TemplateResult {
    if (this.loading) {
      return html`<p class="m-0 text-body text-muted-foreground">Asking the engine.</p>`;
    }
    const stats = this.stats;
    if (stats === null || stats.error) {
      return html`<p class="m-0 text-body text-muted-foreground">
        ${stats?.error ?? 'The engine did not answer.'}
      </p>`;
    }
    return html`
      <dl class="grid gap-x-6 gap-y-2 sm:grid-cols-[max-content_1fr]">
        ${this.rows(stats).map(
          (row) => html`
            <dt class="text-body text-muted-foreground">${row.label}</dt>
            <dd class="m-0 text-body font-medium">${row.value}</dd>
          `,
        )}
      </dl>
    `;
  }
}

/** A size the way an operator reads one. */
function humanBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ['KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let value = n / 1024;
  let at = 0;
  while (value >= 1024 && at < units.length - 1) {
    value /= 1024;
    at += 1;
  }
  return `${value.toFixed(1)} ${units[at]}`;
}

/** Seconds as the largest unit that stays readable. */
function humanDuration(seconds: number): string {
  if (seconds < 60) return `${seconds}s`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h`;
  return `${Math.floor(seconds / 86400)}d`;
}

EngineStats.register('engine-stats');
