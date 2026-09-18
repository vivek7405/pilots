/**
 * What counts as a service's replica, and what is left-over debris.
 *
 * This is the rule the engine already had and the dashboard did not. A real
 * fleet showed it: `website` asks for one instance, had one on its current
 * release and one still on the release before it, and the page read
 * `2/1 instances online` -- two answering, one wanted, and no number a reader
 * could do anything with.
 *
 * The engine never counted the second one. `replicasOf` in
 * `apps/hostd/internal/services/manager.go` pairs service id WITH release id,
 * so the autoscaler cannot see a machine on a superseded release and will
 * never retire it. Counting it as capacity here was the dashboard inventing a
 * replica the engine does not manage.
 *
 * Counterfactual: drop the release filter from `currentReplicas` and the first
 * two tests below read `2/1`, which is the bug this file exists to hold shut.
 */
import assert from 'node:assert/strict';
import { test } from 'node:test';
import { currentReplicas, attachedTo, isStaleReplica, staleCount } from '#modules/services/utils/replicas.ts';
import { groupApps, isOnline } from '#modules/apps/utils/apps.ts';
import type { Machine } from '#modules/machines/types.ts';

const SERVICE = { id: 'svc-1', name: 'website', app: 'webjs', replicas: 1, release_id: 'rel-new' };

/** The fleet shape that produced `2/1`: one current replica, one left behind. */
const MACHINES = [
  { id: 'm-current', service_id: 'svc-1', release_id: 'rel-new', state: 'suspended' },
  { id: 'm-leftover', service_id: 'svc-1', release_id: 'rel-old', state: 'suspended' },
  { id: 'm-other', service_id: 'svc-2', release_id: 'rel-new', state: 'running' },
] as unknown as Machine[];

test('a machine on a superseded release is attached, but is not a replica', () => {
  assert.equal(attachedTo(MACHINES, SERVICE).length, 2, 'both are attached to the service');
  assert.deepEqual(
    currentReplicas(MACHINES, SERVICE).map((m) => m.id),
    ['m-current'],
    'only the one on the current release is capacity',
  );
  assert.equal(staleCount(MACHINES, SERVICE), 1);
  assert.equal(isStaleReplica(MACHINES[1]!, SERVICE), true);
  assert.equal(isStaleReplica(MACHINES[0]!, SERVICE), false);
});

test('the leftover does not push a one-instance service to two online', () => {
  // `isOnline` is what the app card's N/M is built from. Before the narrowing
  // this said true for the wrong reason and the count read 2/1.
  assert.equal(isOnline(SERVICE, attachedTo(MACHINES, SERVICE), []), true, 'the service is up');

  const { apps } = groupApps([SERVICE], MACHINES, {});
  assert.equal(apps.length, 1);
  assert.equal(apps[0]!.online, 1, 'one service online');
  assert.equal(apps[0]!.services.length, 1, 'out of one service, so the card reads 1/1');
});

test('a service that names no release counts everything attached to it', () => {
  // The engine returns nothing here -- `replicasOf` refuses an empty release
  // id -- but a service with a machine and no release row is inconsistent
  // data, and answering `0/1` about a machine that is serving requests is the
  // same lie pointing the other way.
  const noRelease = { id: 'svc-1', name: 'website', replicas: 1 };
  assert.equal(currentReplicas(MACHINES, noRelease).length, 2, 'nothing to compare against, so both count');
  assert.equal(staleCount(MACHINES, noRelease), 0, 'and neither is marked as a leftover');
});

test('a failed leftover does not make a healthy service look failing', () => {
  const withFailedLeftover = [
    { id: 'm-current', service_id: 'svc-1', release_id: 'rel-new', state: 'running' },
    { id: 'm-leftover', service_id: 'svc-1', release_id: 'rel-old', state: 'error' },
  ] as unknown as Machine[];
  const { apps } = groupApps([SERVICE], withFailedLeftover, {});
  assert.equal(apps[0]!.failing, false, 'the engine gave up on a machine nothing routes to any more');
  assert.equal(apps[0]!.online, 1);
});

/**
 * The narrowing excludes only a machine that POSITIVELY names another release.
 *
 * A missing `release_id` on the machine is unknown, not stale. Reading it as
 * stale would hide a live instance from every count and every list on the
 * strength of an absent field -- which is a worse failure than the `2/1` this
 * rule exists to fix, because it removes something that is serving.
 *
 * Counterfactual: compare with `m.release_id === service.release_id` and this
 * service reports no instances at all.
 */
test('a machine that names no release is unknown, not stale', () => {
  const machines = [
    { id: 'm-1', service_id: 'svc-1', state: 'running' },
    { id: 'm-2', service_id: 'svc-1', state: 'suspended' },
  ] as unknown as Machine[];

  assert.equal(currentReplicas(machines, SERVICE).length, 2, 'both still count as replicas');
  assert.equal(staleCount(machines, SERVICE), 0, 'and neither is marked as a leftover');
  assert.equal(isStaleReplica(machines[0]!, SERVICE), false);
});
