/**
 * `curl -fsSL https://pilots.run/install.sh | sh` is an address printed on the
 * site, in every release's notes and in `pilot upgrade`'s own advice, so it
 * is an API and gets a test like one.
 *
 * The second test holds the contract three files each describe in a comment:
 * the release workflow NAMES the assets, and the script and `pilot upgrade`
 * look them up by that name. A rename in one is a green build and an install
 * nobody can complete.
 *
 * Counterfactual: rename the asset in release-pilot.yml to `pilot-${goos}-${goarch}`
 * and the second test fails naming the workflow.
 */
import { test, before } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { bootApp } from '../helpers/app.ts';
import type { TestApp } from '../helpers/app.ts';

const REPO = join(import.meta.dirname, '..', '..', '..', '..');

let app: TestApp;
before(async () => {
  app = await bootApp();
});

test('/install.sh serves the script as readable text, signed out, without touching the fleet', async () => {
  const calls = app.fleet.calls.length;
  const res = await app.handle(new Request('http://localhost/install.sh'));
  assert.equal(res.status, 200);
  // text/plain so a browser shows it: reading the script is the one defence
  // `curl | sh` has, and a shell content type turns that into a download.
  assert.match(res.headers.get('content-type') ?? '', /^text\/plain/);
  const body = await res.text();
  assert.ok(body.startsWith('#!/bin/sh\n'), 'it is piped into sh, so the first line is the interpreter');
  assert.equal(body, readFileSync(join(REPO, 'apps/web/public/install.sh'), 'utf8'), 'the route serves the one copy');
  assert.equal(app.fleet.calls.length, calls, 'an install must not depend on the fleet being up');
});

test('the workflow, the script and pilot upgrade agree on the asset names', () => {
  const holders: Record<string, RegExp[]> = {
    '.github/workflows/release-pilot.yml': [/dist\/pilot_\$\{goos\}_\$\{goarch\}/, /sha256sum pilot_\* > checksums\.txt/],
    'apps/web/public/install.sh': [/asset="pilot_\$\{goos\}_\$\{goarch\}"/, /\/checksums\.txt"/],
    'apps/pilot/internal/cli/upgrade.go': [/"pilot_%s_%s"/, /checksumsAsset = "checksums\.txt"/],
  };
  for (const [file, patterns] of Object.entries(holders)) {
    const source = readFileSync(join(REPO, file), 'utf8');
    for (const pattern of patterns) assert.match(source, pattern, `${file} no longer spells the contract as ${pattern}`);
  }
});
