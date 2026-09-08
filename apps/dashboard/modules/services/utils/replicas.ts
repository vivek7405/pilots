/**
 * Which of a service's machines are its replicas, and which are debris.
 *
 * The engine's answer is the pair (service, CURRENT release):
 * `replicasOf` in `apps/hostd/internal/services/manager.go` keeps a machine
 * only when `ServiceID == serviceID && ReleaseID == releaseID`, and the
 * autoscaler never sees anything else. A machine left behind on a superseded
 * release is therefore not capacity the engine manages -- it will never be
 * scaled down, because the thing that would scale it down cannot see it.
 *
 * The dashboard grouped by `service_id` alone, which counted that leftover as
 * capacity: a service asking for one instance, with one live replica and one
 * machine still on the previous release, rendered `2/1 instances online`. Two
 * answering, one wanted, and no number on the page that a reader could act on.
 *
 * So the COUNT narrows to the current release, matching the engine, while the
 * per-machine lists keep showing everything attached: a leftover still holds a
 * URL and still counts against the org's quota, and hiding it outright would
 * turn a visible oddity into invisible debris. `isStaleReplica` is what those
 * lists mark it with.
 */

import type { Machine } from '#modules/machines/types.ts';

/** The half of a service this module reads. */
export interface ReleasedService {
  id: string;
  release_id?: string;
}

/** Every machine attached to the service, whatever release it carries. */
export function attachedTo<M extends { service_id?: string }>(machines: M[], service: ReleasedService): M[] {
  return machines.filter((m) => m.service_id === service.id);
}

/**
 * The replicas the engine would act on: attached, and on the current release.
 *
 * A service naming NO release is the one case this does not narrow. The engine
 * returns an empty set there (`replicasOf` refuses an empty release id), but a
 * service with no release row and a running machine is inconsistent data, and
 * answering `0/1 instances online` about a machine that is demonstrably
 * serving requests is the same lie in the other direction. With nothing to
 * compare against, everything attached counts.
 */
export function currentReplicas<M extends { service_id?: string; release_id?: string }>(
  machines: M[],
  service: ReleasedService,
): M[] {
  const attached = attachedTo(machines, service);
  if (!service.release_id) return attached;
  return attached.filter((m) => m.release_id === service.release_id);
}

/** A machine attached to the service but left on an older release. */
export function isStaleReplica(machine: { release_id?: string }, service: ReleasedService): boolean {
  if (!service.release_id) return false;
  return machine.release_id !== service.release_id;
}

/** Test and caller convenience: how many attached machines are leftovers. */
export function staleCount(machines: Machine[], service: ReleasedService): number {
  return attachedTo(machines, service).filter((m) => isStaleReplica(m, service)).length;
}
