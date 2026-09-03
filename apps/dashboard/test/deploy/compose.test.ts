/**
 * The deploy shape.
 *
 * Every one of these is a property that fails SILENTLY in production. A
 * database path off the volume loses every account on the next redeploy. Two
 * replicas double-count usage and corrupt one SQLite file. A secret written
 * inline ships in the compose file, the image and the row. A health check on
 * the wrong path cuts traffic over to an instance that is not warm.
 *
 * So the file is parsed and asserted rather than reviewed by eye.
 *
 * Counterfactual: put the admin key inline instead of `secret://` and the
 * "every sensitive value is a secret reference" assertion fails.
 */

import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { test } from 'node:test';
import { APP_DIR } from '../helpers/app.ts';

const compose = readFileSync(join(APP_DIR, 'compose.yaml'), 'utf8');
const dockerfile = readFileSync(join(APP_DIR, 'Dockerfile'), 'utf8');

/** The value of one `key: value` line, ignoring comments. */
function value(key: string): string | null {
  const match = compose.match(new RegExp(`^\\s*${key}:\\s*(.+?)\\s*$`, 'm'));
  return match ? match[1] : null;
}

test('the dashboard runs on pilots.run, a separate apex from workloads', () => {
  assert.equal(value('custom_domain'), 'pilots.run');
  assert.equal(
    compose.includes('pilotrun.app') && !compose.includes('custom_domain: pilotrun.app'),
    true,
    'the workload apex appears only as the API default, never as this service\'s domain',
  );
});

test('exactly one replica, and one warm machine', () => {
  assert.equal(value('replicas'), '1', 'two replicas would share one SQLite file and double-poll usage');
  assert.equal(value('min_machines_running'), '1');
});

test('the database lives on the mounted volume', () => {
  const url = value('DATABASE_URL');
  assert.ok(url?.startsWith('file:/data/'), `DATABASE_URL is ${url}, which is not on the volume`);
  assert.match(compose, /- app-data:\/data/, 'and the volume is mounted at that path');
  assert.match(compose, /^volumes:\s*$/m, 'and declared');
});

test('every sensitive value is a secret reference, never an inline literal', () => {
  for (const key of ['AUTH_SECRET', 'AUTH_GITHUB_SECRET', 'PILOT_ADMIN_KEY', 'PILOT_GITHUB_APP_KEY']) {
    assert.equal(
      value(key),
      `secret://${key}`,
      `${key} must be resolved by the deploy and sealed by hostd, never written here`,
    );
  }
  assert.equal(/pilot_[a-f0-9]{8,}/.test(compose), false, 'no key-shaped literal anywhere in the file');
});

test('the rollout gates on the readiness path, not on any HTTP answer', () => {
  assert.match(compose, /__webjs\/ready/, 'the compose health check');
  assert.match(dockerfile, /__webjs\/ready/, 'and the image health check use the same probe');
  assert.match(compose, /start_period: 40s/, 'with a start period long enough for a cold boot');
});

test('the image builds from the repo root, because the SDK is a workspace', () => {
  assert.match(compose, /context: \.\.\/\.\./, 'the context is the monorepo root');
  assert.match(compose, /dockerfile: apps\/dashboard\/Dockerfile/);
  assert.match(dockerfile, /npm run build --workspace=sdks\/js/, 'and the SDK is compiled into the image');
  assert.match(dockerfile, /FROM node:24/, 'on the Node version the framework pins');
});

test('PILOT_API_URL is present and is a public address, never loopback', () => {
  const url = value('PILOT_API_URL') ?? '';
  assert.ok(url.length > 0, 'the fleet API hostname is required');
  for (const forbidden of ['127.0.0.1', 'localhost', 'fdcc:', '::1']) {
    assert.equal(url.includes(forbidden), false, `a guest cannot reach the host over ${forbidden}`);
  }
});
