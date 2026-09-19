'use strict';
/**
 * The launcher is the whole of what npm users run before the binary, so it is
 * tested as a process: a fake binary in a copy of the package, launched the
 * way npm's bin shim launches it.
 *
 * Counterfactual: drop `process.argv.slice(2)` from bin/pilot.js and the
 * first test fails on the echoed arguments; drop the signal re-raise and the
 * third fails on the parent's exit; go back to spawnSync and the fourth fails,
 * because the launcher dies with the child still running under nobody.
 */
const { test } = require('node:test');
const assert = require('node:assert/strict');
const { spawn, spawnSync } = require('node:child_process');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { binaryFor } = require('../lib/resolve.js');

const PKG = path.join(__dirname, '..');

/** A copy of the package whose vendor/ holds `script` as this machine's binary. */
function packageWith(script, mode = 0o755) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'pilots-npm-'));
  fs.cpSync(path.join(PKG, 'bin'), path.join(dir, 'bin'), { recursive: true });
  fs.cpSync(path.join(PKG, 'lib'), path.join(dir, 'lib'), { recursive: true });
  const found = binaryFor(process.platform, process.arch, path.join(dir, 'vendor'));
  fs.mkdirSync(path.dirname(found.binary), { recursive: true });
  fs.writeFileSync(found.binary, script, { mode });
  return { dir, launcher: path.join(dir, 'bin', 'pilot.js') };
}

test('arguments, stdio and the exit code pass straight through', () => {
  const { launcher } = packageWith('#!/bin/sh\necho "args:$*"\nexit 7\n');
  const res = spawnSync(process.execPath, [launcher, 'deploy', '--json', 'a b'], { encoding: 'utf8' });
  assert.equal(res.stdout, 'args:deploy --json a b\n');
  assert.equal(res.status, 7);
});

test('a binary that lost its mode bits is repaired once and run', () => {
  const { launcher } = packageWith('#!/bin/sh\necho ran\n', 0o644);
  const res = spawnSync(process.execPath, [launcher], { encoding: 'utf8' });
  assert.equal(res.stdout, 'ran\n');
  assert.equal(res.status, 0);
});

test('a child killed by a signal ends the launcher by the same signal', () => {
  const { launcher } = packageWith('#!/bin/sh\nkill -TERM $$\n');
  const res = spawnSync(process.execPath, [launcher]);
  assert.equal(res.signal, 'SIGTERM');
});

test('a SIGTERM sent to the launcher alone reaches the binary', async () => {
  const { launcher } = packageWith('#!/bin/sh\ntrap "echo got-term; exit 9" TERM\necho ready\nwhile :; do sleep 0.1; done\n');
  const child = spawn(process.execPath, [launcher], { stdio: ['ignore', 'pipe', 'inherit'] });
  let stdout = '';
  const ended = new Promise((resolve) => child.on('exit', (code, signal) => resolve({ code, signal })));
  await new Promise((resolve) => {
    child.stdout.on('data', (chunk) => {
      stdout += chunk;
      if (stdout.includes('ready')) resolve();
    });
  });
  child.kill('SIGTERM');
  const { code, signal } = await ended;
  assert.match(stdout, /got-term/, 'the binary never saw the signal');
  assert.equal(signal, null);
  assert.equal(code, 9, 'the launcher ends the way the binary chose to');
});

test('an incomplete package says to reinstall, not ENOENT on a path in node_modules', () => {
  const { dir, launcher } = packageWith('');
  fs.rmSync(path.join(dir, 'vendor'), { recursive: true });
  const res = spawnSync(process.execPath, [launcher], { encoding: 'utf8' });
  assert.equal(res.status, 1);
  assert.match(res.stderr, /reinstall it \(npm install -g pilots\)/);
});

test('every platform maps to the release asset name, or to a reason', () => {
  const at = (platform, arch) => binaryFor(platform, arch, '/v');
  assert.deepEqual(at('linux', 'x64'), { binary: '/v/pilot_linux_amd64' });
  assert.deepEqual(at('linux', 'arm64'), { binary: '/v/pilot_linux_arm64' });
  assert.deepEqual(at('darwin', 'x64'), { binary: '/v/pilot_darwin_amd64' });
  assert.deepEqual(at('darwin', 'arm64'), { binary: '/v/pilot_darwin_arm64' });

  // Windows is told what to do instead, by name, on every architecture.
  for (const arch of ['x64', 'arm64', 'ia32']) {
    const windows = at('win32', arch);
    assert.match(windows.error, /no Windows build/);
    assert.match(windows.error, /WSL/);
    assert.match(windows.error, /pilots\.run\/install/);
  }
  assert.match(at('linux', 'riscv64').error, /no build for linux\/riscv64/);
  assert.match(at('freebsd', 'x64').error, /no build for freebsd\/x64/);
});

test('the package ships what the launcher needs, and both command names', () => {
  const pkg = JSON.parse(fs.readFileSync(path.join(PKG, 'package.json'), 'utf8'));
  assert.deepEqual(pkg.bin, { pilot: 'bin/pilot.js', pilots: 'bin/pilot.js' });
  for (const needed of ['bin', 'lib', 'vendor']) assert.ok(pkg.files.includes(needed), `files is missing ${needed}`);
  // No install script may ever download the binary: npm 12 runs none by default.
  for (const hook of ['preinstall', 'install', 'postinstall']) assert.equal(pkg.scripts?.[hook], undefined, `${hook} script`);
  assert.equal(pkg.dependencies, undefined);
});
