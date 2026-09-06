/**
 * <relative-time datetime="...">: an absolute timestamp that reads as an age.
 *
 * The absolute time is what the SERVER renders, inside a real `<time>` element
 * carrying the machine-readable value. That matters twice: the page is correct
 * with scripting off, and the exact moment is never lost, because the browser
 * only ever replaces the TEXT and the `title` keeps the absolute form.
 *
 * One shared interval on `globalThis` re-renders every mounted instance, not
 * one timer each. A machines list has a timestamp per row, and a page with
 * forty timers wakes the CPU forty times a minute to change forty strings that
 * all changed for the same reason.
 */

import { WebComponent, html, prop } from '@webjsdev/core';
import { epochMs } from '#lib/utils/time.ts';

const TICK_MS = 30_000;

interface TimeBus {
  ticks: Set<() => void>;
  timer?: ReturnType<typeof setInterval>;
}

function bus(): TimeBus {
  const g = globalThis as { __pilotsTimeBus?: TimeBus };
  return (g.__pilotsTimeBus ??= { ticks: new Set() });
}

/** `2026-09-06 13:31` in UTC: unambiguous, and the same for every reader. */
export function absolute(value: number | string | undefined): string {
  const date = toDate(value);
  if (!date) return '';
  return date.toISOString().slice(0, 16).replace('T', ' ') + ' UTC';
}

/** The ISO string a `<time datetime>` needs, or '' when there is no time. */
export function isoOf(value: number | string | undefined): string {
  return toDate(value)?.toISOString() ?? '';
}

/**
 * An epoch stamp reaches an element as an attribute, which is a STRING.
 * `new Date("1788699180")` is an Invalid Date, because a string goes down the
 * date-string parser and never the timestamp one, so a numeric string is
 * converted before it is handed over. Without this every time on every page
 * read `never`.
 *
 * The unit is then settled by `epochMs`: the engine stamps SECONDS
 * (`time.Now().Unix()`), and reading those as milliseconds put every timestamp
 * in January 1970 and made every age read "56 years ago".
 */
function toDate(value: number | string | undefined): Date | null {
  if (value === undefined || value === null || value === '') return null;
  const raw = typeof value === 'number' ? value : /^-?\d+$/.test(value.trim()) ? Number(value) : NaN;
  const date = Number.isNaN(raw) ? new Date(value) : new Date(epochMs(raw));
  return Number.isNaN(date.getTime()) ? null : date;
}

const UNITS: [limit: number, seconds: number, name: string][] = [
  [60, 1, 'second'],
  [3600, 60, 'minute'],
  [86_400, 3600, 'hour'],
  [604_800, 86_400, 'day'],
  [2_629_800, 604_800, 'week'],
  [31_557_600, 2_629_800, 'month'],
  [Infinity, 31_557_600, 'year'],
];

/** `just now`, `3 min ago`, `8 hours ago`, `2 weeks ago`, `in 5 minutes`. */
export function ago(value: number | string | undefined, now = Date.now()): string {
  const date = toDate(value);
  if (!date) return '';
  const deltaSec = (now - date.getTime()) / 1000;
  const abs = Math.abs(deltaSec);
  if (abs < 10) return 'just now';
  for (const [limit, seconds, name] of UNITS) {
    if (abs < limit) {
      const n = Math.floor(abs / seconds);
      const unit = n === 1 ? name : `${name}s`;
      return deltaSec >= 0 ? `${n} ${unit} ago` : `in ${n} ${unit}`;
    }
  }
  return '';
}

export class RelativeTime extends WebComponent({
  datetime: prop(String),
  /** Set once the browser has taken over, so SSR keeps the absolute form. */
  live: prop(Boolean, { state: true }),
}) {
  #tick = () => this.requestUpdate();

  constructor() {
    super();
    this.datetime = '';
    this.live = false;
  }

  connectedCallback() {
    super.connectedCallback();
    this.live = true;
    const b = bus();
    b.ticks.add(this.#tick);
    b.timer ??= setInterval(() => {
      for (const tick of bus().ticks) tick();
    }, TICK_MS);
  }

  disconnectedCallback() {
    super.disconnectedCallback();
    const b = bus();
    b.ticks.delete(this.#tick);
    if (b.ticks.size === 0 && b.timer) {
      clearInterval(b.timer);
      b.timer = undefined;
    }
  }

  render() {
    const iso = isoOf(this.datetime);
    if (!iso) return html`<time class="text-muted-foreground">never</time>`;
    const exact = absolute(this.datetime);
    return html`<time datetime=${iso} title=${exact}>${this.live ? ago(this.datetime) : exact}</time>`;
  }
}
RelativeTime.register('relative-time');
