/**
 * `curl -fsSL https://pilots.run/install.sh | sh` is an address printed on the
 * site, in every release's notes and in `pilot upgrade`'s own advice, so it
 * is an API and gets a test like one.
 *
 * The two in the middle run the script itself; their own comment says why.
 *
 * The last test holds the contract three files each describe in a comment:
 * the release workflow NAMES the assets, and the script and `pilot upgrade`
 * look them up by that name. A rename in one is a green build and an install
 * nobody can complete.
 *
 * Counterfactual: rename the asset in release-pilot.yml to `pilot-${goos}-${goarch}`
 * and the last test fails naming the workflow.
 */
import { test, before } from 'node:test';
import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
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

/**
 * The script run for real, against a stand-in release: a `curl` first on PATH
 * that serves a directory, so nothing here touches the network. It holds the
 * one sentence the script's verification comment turns on, that there is ONE
 * outcome that installs.
 *
 * Counterfactual: put `2>/dev/null` back on the checksums fetch and drop its
 * `|| die`, and the last two cases install a binary nothing verified.
 */
const SCRIPT = join(REPO, 'apps/web/public/install.sh');
const CURL_SHIM = `#!/bin/sh
out=""; url=""
while [ $# -gt 0 ]; do
  case "$1" in -o) out="$2"; shift 2 ;; -*) shift ;; *) url="$1"; shift ;; esac
done
case "$url" in */releases/latest) src="$REL/latest.json" ;; *) src="$REL/\${url##*/}" ;; esac
[ -f "$src" ] || { echo "curl: (22) 404 $url" >&2; exit 22; }
if [ -n "$out" ]; then cat "$src" > "$out"; else cat "$src"; fi
`;

function install(checksums: (asset: string, digest: string) => string | null) {
  const goos = process.platform === 'darwin' ? 'darwin' : 'linux';
  const goarch = process.arch === 'arm64' ? 'arm64' : 'amd64';
  const asset = `pilot_${goos}_${goarch}`;
  const root = mkdtempSync(join(tmpdir(), 'pilot-install-'));
  const [shim, rel, bin] = ['shim', 'rel', 'bin'].map((d) => join(root, d));
  for (const d of [shim, rel, bin]) mkdirSync(d);
  writeFileSync(join(shim, 'curl'), CURL_SHIM, { mode: 0o755 });
  const binary = '#!/bin/sh\necho v9.9.9\n';
  writeFileSync(join(rel, asset), binary);
  writeFileSync(
    join(rel, 'latest.json'),
    JSON.stringify({ assets: [asset, 'checksums.txt'].map((n) => ({ browser_download_url: `https://release.test/dl/${n}` })) }, null, 1),
  );
  const sums = checksums(asset, createHash('sha256').update(binary).digest('hex'));
  if (sums !== null) writeFileSync(join(rel, 'checksums.txt'), sums);
  const run = spawnSync('sh', [SCRIPT], {
    env: { ...process.env, PATH: `${shim}:${process.env.PATH}`, REL: rel, PILOT_BIN_DIR: bin },
    encoding: 'utf8',
  });
  const left = readdirSync(bin);
  rmSync(root, { recursive: true, force: true });
  return { status: run.status, stdout: run.stdout, stderr: run.stderr, left };
}

const unix = process.platform === 'linux' || process.platform === 'darwin';

test('install.sh installs a download whose digest matches, in either sha256sum line format', { skip: !unix }, () => {
  for (const sep of ['  ', ' *']) {
    const run = install((asset, digest) => `${'0'.repeat(64)}  pilot_other_arch\n${digest}${sep}${asset}\n`);
    assert.equal(run.status, 0, run.stderr);
    assert.match(run.stdout, /pilot: sha256 verified\npilot: installed v9\.9\.9/);
    assert.deepEqual(run.left, ['pilot']);
  }
});

test('install.sh installs nothing, and leaves nothing behind, when it cannot verify', { skip: !unix }, () => {
  const cases: Array<[string, Parameters<typeof install>[0], RegExp]> = [
    ['a wrong digest', (asset) => `${'0'.repeat(64)}  ${asset}\n`, /checksum mismatch/],
    ['no line for the asset', (_asset, digest) => `${digest}  pilot_other_arch\n`, /carries no checksum for pilot_/],
    ['no checksums.txt at all', () => null, /could not fetch .*checksums\.txt/],
  ];
  for (const [name, checksums, message] of cases) {
    const run = install(checksums);
    assert.equal(run.status, 1, `${name}: ${run.stdout}`);
    assert.match(run.stderr, message, name);
    assert.doesNotMatch(run.stdout, /verified|installed/, name);
    assert.deepEqual(run.left, [], `${name}: nothing, not even a temp file, may be left in BIN_DIR`);
  }
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
