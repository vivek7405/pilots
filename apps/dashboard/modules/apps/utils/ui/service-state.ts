/**
 * A service's state as one phrase, from the state of its instances.
 *
 * Its own module because two things render it: the canvas card, and
 * `<live-status>` when the feed replaces that card's line. Importing it from
 * the card would be a cycle -- the card imports the element so a canvas cannot
 * be drawn without it -- and a second copy would drift from this one the first
 * time either was edited.
 */

import { html } from '@webjsdev/core';
import type { TemplateResult } from '@webjsdev/core';
import type { Machine } from '#modules/machines/types.ts';
import { statusDot, toneDot } from '#modules/machines/utils/ui/state.ts';
import { stateSince } from '#modules/machines/utils/ui/status-line.ts';

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
export function serviceStatus(replicas: Machine[]): TemplateResult {
  if (replicas.length === 0) return html`<span class="inline-flex items-center gap-1.5">${toneDot('muted')} <span>No instances</span></span>`;

  const failed = replicas.filter((r) => r.state === 'error' || r.state === 'failed');
  if (failed.length > 0) return stateSince('error', edge(failed, (r) => r.last_activity ?? r.last_start_at, 'last'));

  const running = replicas.filter((r) => r.state === 'running');
  if (running.length > 0) return stateSince('running', edge(running, (r) => r.last_start_at ?? r.created_at, 'first'));

  if (replicas.every((r) => r.state === 'suspended')) {
    return stateSince('suspended', edge(replicas, (r) => r.last_activity ?? r.last_start_at, 'last'));
  }

  if (replicas.some((r) => r.state === 'creating' || r.state === 'starting')) return stateSince('starting', undefined);
  return statusDot(replicas[0]!.state);
}
