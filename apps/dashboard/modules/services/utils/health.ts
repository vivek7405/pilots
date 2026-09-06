/**
 * Whether a service is in trouble, decided from fields the API already
 * returns.
 *
 * This exists because of a real morning: the gallery service failed its health
 * gate and stayed failed for a day, and the dashboard's answer was a 502 on
 * the URL. Nothing on any page said the release had never passed, which
 * replicas were involved, or what to look at next.
 *
 * Pure and unit-tested. It renders nothing: the pills and the doctor card are
 * separate so this rule can be asserted without parsing markup.
 */

import type { Machine } from '#modules/machines/types.ts';
import { epochMs } from '#lib/utils/time.ts';

export interface HealthRelease {
  id: string;
  healthy?: boolean;
  created_at?: number;
}

export interface HealthService {
  id: string;
  release_id?: string;
  /** The health block on the service, whose `grace` is the gate's window. */
  health?: { grace?: number } | null;
}

export type HealthPill = 'failing' | 'suspended';

export interface ServiceHealth {
  pills: HealthPill[];
  /** Ids of the replicas that are the evidence, for the doctor card to name. */
  failing: string[];
  /** The current release, when the service names one that exists. */
  release?: HealthRelease;
  /** The gate window in seconds, defaulted the way the engine defaults it. */
  graceSec: number;
}

const DEFAULT_GRACE_SEC = 40;

/**
 * `now` is a parameter rather than a `Date.now()` default, so this module has
 * no call of any kind at module scope. A call there reads as browser work to
 * the elision pass, which pins every page that imports this file and ships it,
 * and with it every display-only component the page renders.
 */
export function serviceHealth(
  service: HealthService,
  replicas: Machine[],
  releases: HealthRelease[],
  now?: number,
): ServiceHealth {
  const at = now ?? Date.now();
  const release = releases.find((r) => r.id === service.release_id);
  const graceSec = service.health?.grace ?? DEFAULT_GRACE_SEC;
  const pills: HealthPill[] = [];
  const failing: string[] = [];

  // A replica the engine gave up on is evidence on its own, whatever the
  // release says: it is not serving and it is not asleep.
  for (const replica of replicas) {
    if (replica.state === 'error') failing.push(replica.id);
  }

  // A release that has not passed its gate is only news once the gate's own
  // window has elapsed. Before that it is a deploy in progress, and calling it
  // a failure would make every deploy flash red on its way up.
  // `created_at` is the engine's own stamp, which is SECONDS; `at` is
  // milliseconds. Comparing the two raw made the difference about 1.8e12 ms,
  // so every release was instantly "past its grace" and a deploy still coming
  // up was reported as a failure the moment a replica was not yet running.
  const past = release?.created_at !== undefined && at - epochMs(release.created_at) > graceSec * 1000;
  if (release && release.healthy === false && past) {
    const ofRelease = replicas.filter((r) => r.release_id === release.id);
    const notRunning = ofRelease.filter((r) => r.state !== 'running');
    if (notRunning.length > 0) {
      for (const replica of notRunning) if (!failing.includes(replica.id)) failing.push(replica.id);
    }
  }
  if (failing.length > 0) pills.push('failing');

  // Every replica asleep is not a fault, but it is why the URL is slow to
  // answer, and a reader looking at a quiet service deserves to know it.
  if (replicas.length > 0 && replicas.every((r) => r.state === 'suspended')) pills.push('suspended');

  return { pills, failing, release, graceSec };
}
