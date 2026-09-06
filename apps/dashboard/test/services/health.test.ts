/**
 * When a service is in trouble, and what the doctor card says about it.
 *
 * This rule exists because of a specific morning: the gallery service failed
 * its health gate and stayed failed for a day, and the dashboard's only signal
 * was a 502 on the URL. Every assertion here is a thing that was invisible.
 *
 * The grace window is the subtle half. A release that has not passed yet is a
 * deploy in progress, not a failure, so a service must not flash red on its
 * way up. Counterfactual: drop the elapsed-time check and the third test goes
 * red, which is the version that cries wolf on every deploy.
 */

import assert from 'node:assert/strict';
import { test } from 'node:test';
import { renderToString } from '@webjsdev/core/server';
import { serviceHealth } from '#modules/services/utils/health.ts';
import { healthPills } from '#modules/services/utils/ui/health-pills.ts';
import { doctorCard } from '#modules/services/utils/ui/doctor-card.ts';
import type { Machine } from '#modules/machines/types.ts';

const NOW = Date.parse('2026-09-01T12:00:00Z');
const MINUTE = 60_000;

const service = { id: 'svc-1', release_id: 'rel-2' };
const replica = (id: string, state: string, release = 'rel-2'): Machine => ({
  id,
  name: id,
  state,
  release_id: release,
  service_id: 'svc-1',
});

test('a healthy service has no pills and names no replicas', () => {
  const health = serviceHealth(
    service,
    [replica('m-1', 'running'), replica('m-2', 'running')],
    [{ id: 'rel-2', healthy: true, created_at: NOW - 10 * MINUTE }],
    NOW,
  );
  assert.deepEqual(health.pills, []);
  assert.deepEqual(health.failing, []);
});

test('a release past its grace with a replica not running is failing', () => {
  const health = serviceHealth(
    service,
    [replica('m-1', 'running'), replica('m-2', 'creating')],
    [{ id: 'rel-2', healthy: false, created_at: NOW - 10 * MINUTE }],
    NOW,
  );
  assert.deepEqual(health.pills, ['failing']);
  assert.deepEqual(health.failing, ['m-2']);
  assert.equal(health.graceSec, 40, 'the engine default when the service declares none');
});

test('a deploy still inside its grace window is not a failure', () => {
  // Twenty seconds into a forty-second gate: the release has not passed, and
  // it has not failed either. Calling this red makes every deploy flash red.
  const health = serviceHealth(
    service,
    [replica('m-1', 'creating')],
    [{ id: 'rel-2', healthy: false, created_at: NOW - 20_000 }],
    NOW,
  );
  assert.deepEqual(health.pills, []);
});

test('a replica the engine gave up on is evidence whatever the release says', () => {
  const health = serviceHealth(
    service,
    [replica('m-1', 'error')],
    [{ id: 'rel-2', healthy: true, created_at: NOW - 10 * MINUTE }],
    NOW,
  );
  assert.deepEqual(health.pills, ['failing']);
  assert.deepEqual(health.failing, ['m-1']);
});

test('every replica asleep is reported, because it is why the URL is slow', () => {
  const health = serviceHealth(
    service,
    [replica('m-1', 'suspended'), replica('m-2', 'suspended')],
    [{ id: 'rel-2', healthy: true, created_at: NOW - 10 * MINUTE }],
    NOW,
  );
  assert.deepEqual(health.pills, ['suspended']);
});

test('a custom grace on the service is honoured over the engine default', () => {
  const slow = { id: 'svc-1', release_id: 'rel-2', health: { grace: 600 } };
  const health = serviceHealth(
    slow,
    [replica('m-1', 'creating')],
    [{ id: 'rel-2', healthy: false, created_at: NOW - 5 * MINUTE }],
    NOW,
  );
  assert.deepEqual(health.pills, [], 'five minutes into a ten-minute gate is still in progress');
  assert.equal(health.graceSec, 600);
});

test('the pills name the failure in words, not only in colour', async () => {
  const health = serviceHealth(
    service,
    [replica('m-1', 'error')],
    [{ id: 'rel-2', healthy: false, created_at: NOW - 10 * MINUTE }],
    NOW,
  );
  const out = await renderToString(healthPills(health));
  assert.match(out, /Replicas failing health checks/);
});

test('the doctor card names the replicas, the checks and the next command', async () => {
  const replicas = [replica('m-1', 'error')];
  const health = serviceHealth(service, replicas, [{ id: 'rel-2', healthy: false, created_at: NOW - 10 * MINUTE }], NOW);
  const out = await renderToString(doctorCard({ health, replicas, serviceName: 'web' }));

  assert.match(out, /has not passed its health check in 40 s/);
  assert.match(out, /href="\/machines\/m-1"/, 'the failing replica is a link, not a mention');
  assert.match(out, /PORT/);
  assert.match(out, /secret:\/\//);
  assert.match(out, /pilot machines logs m-1/);
});

test("the card prefers the engine's own words when a deploy just failed", async () => {
  const replicas = [replica('m-1', 'error')];
  const health = serviceHealth(service, replicas, [{ id: 'rel-2', healthy: false, created_at: NOW - 10 * MINUTE }], NOW);
  const out = await renderToString(
    doctorCard({ health, replicas, serviceName: 'web', symptom: 'health gate failed: 502 from /' }),
  );
  assert.match(out, /health gate failed: 502 from \//);
  assert.ok(!out.includes('has not passed its health check in 40 s'), 'the specific error replaces the generic one');
});

test('a healthy service renders no card at all', async () => {
  const health = serviceHealth(service, [replica('m-1', 'running')], [{ id: 'rel-2', healthy: true }], NOW);
  assert.equal(doctorCard({ health, replicas: [], serviceName: 'web' }), '');
  assert.equal(healthPills(health), '');
});
