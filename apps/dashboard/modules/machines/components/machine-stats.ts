/**
 * What one instance is using, refreshed while the page is open.
 *
 * # Why CPU is shown as a percentage and the API returns a total
 *
 * The API returns a monotonic total, which is the only shape that survives a
 * suspend without lying. A percentage is what a person reads, and it needs two
 * readings to exist at all, so it is computed HERE from consecutive samples
 * rather than on the server, where there is nothing to compare against.
 *
 * The first render therefore shows no percentage, and says so rather than
 * showing zero: a zero that means "not measured yet" and a zero that means
 * "idle" are the same pixel, and the wrong one sends somebody looking for a
 * problem that is not there.
 */
import { WebComponent, html, prop } from '@webjsdev/core';
import type { TemplateResult } from '@webjsdev/core';

interface Sample {
  state?: string;
  vcpus?: number;
  cpu_seconds?: number;
  memory_bytes?: number;
  memory_limit_bytes?: number;
  sampled_at?: number;
  error?: string;
}

const POLL_MS = 5000;

export class MachineStats extends WebComponent({
  machineId: String,
  sample: prop<Sample | null>(Object),
  cpuPercent: Number,
  loaded: Boolean,
}) {
  constructor() {
    super();
    this.machineId = '';
    this.sample = null;
    this.cpuPercent = -1;
    this.loaded = false;
  }

  private timer: ReturnType<typeof setInterval> | undefined;
  private previous: Sample | undefined;

  connectedCallback(): void {
    super.connectedCallback?.();
    void this.load();
    this.timer = setInterval(() => void this.load(), POLL_MS);
  }

  disconnectedCallback(): void {
    super.disconnectedCallback?.();
    // Cleared, or a panel opened and closed a dozen times leaves a dozen
    // pollers running against a machine nobody is looking at.
    if (this.timer) clearInterval(this.timer);
    this.timer = undefined;
  }

  private async load(): Promise<void> {
    try {
      const res = await fetch(`/api/machines/${encodeURIComponent(this.machineId)}/metrics`);
      const next = (await res.json()) as Sample;
      this.cpuPercent = ratio(this.previous, next);
      this.previous = next;
      this.sample = next;
    } catch {
      this.sample = { error: 'could not read this instance' };
    } finally {
      this.loaded = true;
    }
  }

  render(): TemplateResult {
    if (!this.loaded) {
      return html`<p class="m-0 text-body text-muted-foreground">Reading.</p>`;
    }
    const sample = this.sample;
    if (!sample || sample.error) {
      return html`<p class="m-0 text-body text-muted-foreground">
        ${sample?.error ?? 'No reading.'}
      </p>`;
    }

    const limit = sample.memory_limit_bytes ?? 0;
    const held = sample.memory_bytes ?? 0;
    const share = limit > 0 ? Math.min(100, (held / limit) * 100) : 0;

    return html`
      <dl class="grid gap-x-6 gap-y-2 sm:grid-cols-[max-content_1fr]">
        <dt class="text-body text-muted-foreground">CPU</dt>
        <dd class="m-0 text-body font-medium">
          ${this.cpuPercent < 0
            ? html`<span class="text-muted-foreground">Measuring, one moment</span>`
            : `${this.cpuPercent.toFixed(1)}% of ${sample.vcpus ?? 1} vCPU`}
        </dd>

        <dt class="text-body text-muted-foreground">Memory</dt>
        <dd class="m-0 grid gap-1 text-body font-medium">
          <span>${bytes(held)}${limit > 0 ? ` of ${bytes(limit)}` : ''}</span>
          ${limit > 0
            ? html`<span
                class="block h-1.5 w-full max-w-xs overflow-hidden rounded-full bg-muted"
                role="img"
                aria-label=${`${share.toFixed(0)} percent of the memory ceiling`}
              >
                <span
                  class=${share >= 90 ? 'block h-full bg-destructive' : 'block h-full bg-primary'}
                  style=${`width: ${share}%`}
                ></span>
              </span>`
            : ''}
          ${share >= 90
            ? html`<span class="text-meta text-destructive">
                Near the ceiling. The next allocation may be the one that fails.
              </span>`
            : ''}
        </dd>

        <dt class="text-body text-muted-foreground">Total CPU</dt>
        <dd class="m-0 text-body font-medium">${(sample.cpu_seconds ?? 0).toFixed(1)}s</dd>
      </dl>
    `;
  }
}

/**
 * The CPU share between two samples, or -1 when there is no pair yet.
 *
 * -1 rather than 0, because "not measured yet" and "idle" must not render as
 * the same thing. A total that went backwards also yields -1: that should not
 * happen, and showing a negative percentage would be worse than saying nothing.
 */
function ratio(previous: Sample | undefined, next: Sample): number {
  if (!previous) return -1;
  const elapsed = (next.sampled_at ?? 0) - (previous.sampled_at ?? 0);
  if (elapsed <= 0) return -1;
  const used = (next.cpu_seconds ?? 0) - (previous.cpu_seconds ?? 0);
  if (used < 0) return -1;
  const cores = Math.max(1, next.vcpus ?? 1);
  return (used / (elapsed * cores)) * 100;
}

function bytes(n: number): string {
  if (n < 1024) return `${n} B`;
  const units = ['KiB', 'MiB', 'GiB', 'TiB'];
  let value = n / 1024;
  let at = 0;
  while (value >= 1024 && at < units.length - 1) {
    value /= 1024;
    at += 1;
  }
  return `${value.toFixed(1)} ${units[at]}`;
}

MachineStats.register('machine-stats');
