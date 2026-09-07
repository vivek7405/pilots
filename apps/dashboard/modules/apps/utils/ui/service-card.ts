/**
 * One service on the canvas: a card with its name, where it answers, whether
 * it is up, and the storage it mounts.
 *
 * It is a plain link. Clicking it opens the service's panel by putting the
 * selection in the URL, so the panel exists with scripting off, a reload
 * restores it and a modifier click opens it in a new tab. Below the `sm`
 * breakpoint the cards stack in reading order; from `sm` up they take the
 * positions the layout computed and the arrows are drawn between them.
 */

import { html } from '@webjsdev/core';
import type { TemplateResult } from '@webjsdev/core';
import { statusDot, toneDot } from '#modules/machines/utils/ui/state.ts';
import { when } from '#modules/machines/utils/ui/status-line.ts';
import type { PlacedNode } from '#modules/apps/utils/layout.ts';
import type { Machine } from '#modules/machines/types.ts';
import { cn } from '#lib/utils/cn.ts';
// The card declares its own dependency: an un-upgraded <relative-time> renders
// nothing at all, so a page that shows a card without importing this would
// print "Sleeping since" with the time silently missing.
import '#components/relative-time.ts';

export interface CardService {
  id: string;
  name: string;
  url?: string;
  volume_id?: string;
}

export interface CardVolume {
  id: string;
  name: string;
  size_gib: number;
}

/** The earliest or latest of a stamp across replicas, ignoring the unstamped. */
function edge(replicas: Machine[], pick: (m: Machine) => number | undefined, side: 'first' | 'last'): number | undefined {
  const stamps = replicas.map(pick).filter((v): v is number => typeof v === 'number');
  if (stamps.length === 0) return undefined;
  return side === 'first' ? Math.min(...stamps) : Math.max(...stamps);
}

/**
 * The state, and since when.
 *
 * Same shape as a sandbox's status line, because a reader who has learned
 * "Sleeping since 2 hours ago" on the sandboxes list should not have to learn
 * a second vocabulary here. What differs is that a service is SEVERAL
 * machines, so "since" needs a definition rather than a field:
 *
 * - Running takes the EARLIEST start among the running replicas, which is how
 *   long the service has been continuously answering. The latest would reset
 *   the clock every time one replica was replaced, during a rolling deploy
 *   that never dropped a request.
 * - Sleeping takes the LATEST activity across them all: the service went idle
 *   when its last awake replica did, not when its first one did.
 *
 * A missing stamp prints the word alone. "Sleeping since" with nothing after
 * it is worse than "Sleeping", the same rule the sandbox status line follows.
 */
function serviceStatus(replicas: Machine[]): TemplateResult {
  if (replicas.length === 0) return html`<span class="inline-flex items-center gap-1.5">${toneDot('muted')} <span>No instances</span></span>`;

  const failed = replicas.filter((r) => r.state === 'error' || r.state === 'failed');
  if (failed.length > 0) {
    const at = edge(failed, (r) => r.last_activity ?? r.last_start_at, 'last');
    // "Failed 2 hours ago", not "Failed since": the failure was a moment.
    return at === undefined ? statusDot('error') : html`${statusDot('error')} ${when(at)}`;
  }

  const running = replicas.filter((r) => r.state === 'running');
  if (running.length > 0) return since(statusDot('running'), edge(running, (r) => r.last_start_at ?? r.created_at, 'first'));

  if (replicas.every((r) => r.state === 'suspended')) {
    return since(statusDot('suspended'), edge(replicas, (r) => r.last_activity ?? r.last_start_at, 'last'));
  }

  // Starting and creating are moments, not durations. A clock on them counts
  // up for a few seconds and then the card says something else.
  if (replicas.some((r) => r.state === 'creating' || r.state === 'starting')) return statusDot('starting');
  return statusDot(replicas[0]!.state);
}

/** The state word, then `since <time>` when there is a time to name. */
function since(dot: TemplateResult, at: number | undefined): TemplateResult {
  return at === undefined ? dot : html`${dot} since ${when(at)}`;
}

function host(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

export function serviceCard(opts: {
  app: string;
  service: CardService;
  placed: PlacedNode;
  replicas: Machine[];
  volume?: CardVolume;
  selected: boolean;
}): TemplateResult {
  const { app, service, placed, replicas, volume, selected } = opts;
  const href = `/apps/${encodeURIComponent(app)}?service=${encodeURIComponent(service.id)}`;
  // A service with no domain has no stable URL, but its running instance has a
  // routable name-based one. Show that so the card is never a dead end; it is
  // the current instance's address and moves if the instance is replaced.
  const liveUrl = service.url || replicas.find((r) => r.url)?.url || '';
  return html`<a
    href=${href}
    data-canvas-card
    aria-current=${selected ? 'true' : 'false'}
    class=${cn(
      'flex flex-col gap-0.5 rounded-xl border border-border bg-card px-4 py-3.5 text-card-foreground no-underline shadow-sm transition-colors hover:border-border-strong',
      'sm:absolute sm:h-30 sm:w-60',
      selected ? 'ring-2 ring-primary' : '',
    )}
    style=${`left:${placed.x}px;top:${placed.y}px`}
  >
    <span class="truncate text-body font-semibold leading-tight">${service.name}</span>
    <span class="truncate text-meta leading-tight text-muted-foreground">${liveUrl ? host(liveUrl) : 'No URL yet'}</span>
    <!--
      truncate, because the card is a fixed 15rem wide from the sm breakpoint
      up and the phrase now carries a timestamp. Without it "Sleeping since 3
      weeks ago" wraps to a second line and pushes the volume row out of the
      card. (No backticks in this comment: it sits inside a template literal,
      so one would end it.)
    -->
    <span class="mt-auto truncate text-meta leading-tight">${serviceStatus(replicas)}</span>
    ${volume
      ? html`<span class="mt-1 flex items-center gap-1.5 border-t border-border pt-1.5 text-meta leading-tight text-muted-foreground">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="size-3.5 shrink-0" aria-hidden="true"><path d="M22 12H2M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z"/><path d="M6 16h.01M10 16h.01"/></svg>
          <span class="truncate">${volume.name}</span>
          <span class="ml-auto whitespace-nowrap">${volume.size_gib} GB</span>
        </span>`
      : ''}
  </a>`;
}
