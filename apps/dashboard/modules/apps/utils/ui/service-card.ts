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
import type { PlacedNode } from '#modules/apps/utils/layout.ts';
import type { Machine } from '#modules/machines/types.ts';
import { cn } from '#lib/utils/cn.ts';

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

/** The one word a service's instances add up to. */
function serviceStatus(replicas: Machine[]): TemplateResult {
  if (replicas.length === 0) return html`<span class="inline-flex items-center gap-1.5">${toneDot('muted')} <span>No instances</span></span>`;
  if (replicas.some((r) => r.state === 'error' || r.state === 'failed')) return statusDot('error');
  if (replicas.some((r) => r.state === 'running')) return statusDot('running');
  if (replicas.every((r) => r.state === 'suspended')) return statusDot('suspended');
  if (replicas.some((r) => r.state === 'creating' || r.state === 'starting')) return statusDot('starting');
  return statusDot(replicas[0]!.state);
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
    <span class="mt-auto text-meta leading-tight">${serviceStatus(replicas)}</span>
    ${volume
      ? html`<span class="mt-1 flex items-center gap-1.5 border-t border-border pt-1.5 text-meta leading-tight text-muted-foreground">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" class="size-3.5 shrink-0" aria-hidden="true"><path d="M22 12H2M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z"/><path d="M6 16h.01M10 16h.01"/></svg>
          <span class="truncate">${volume.name}</span>
          <span class="ml-auto whitespace-nowrap">${volume.size_gib} GB</span>
        </span>`
      : ''}
  </a>`;
}
