#!/usr/bin/env node
'use strict';
/**
 * The `pilot` (and `pilots`, for `npx pilots ...`) command as npm installs it:
 * find this machine's binary in vendor/ and become it, as nearly as Node can.
 *
 * Node has no exec(), so this process stays as the parent. It hands the child
 * its stdio untouched (the console and the TUI put the terminal in raw mode
 * and must own it), waits, and then ends the way the child ended: the same
 * exit code, or the same signal re-raised on itself, so a shell or a CI step
 * above sees exactly what it would have seen from the binary run directly.
 */
const { spawnSync } = require('node:child_process');
const fs = require('node:fs');
const { binaryFor } = require('../lib/resolve.js');

const found = binaryFor(process.platform, process.arch);
if ('error' in found) {
  process.stderr.write(found.error + '\n');
  process.exit(1);
}

function run() {
  return spawnSync(found.binary, process.argv.slice(2), { stdio: 'inherit' });
}

let result = run();
if (result.error && result.error.code === 'EACCES') {
  // A tarball extracted by a tool that drops the mode bits. Worth one repair
  // rather than an error that sends someone to chmod inside node_modules.
  try {
    fs.chmodSync(found.binary, 0o755);
    result = run();
  } catch {
    // Fall through to the error below with the original cause.
  }
}
if (result.error) {
  const why = result.error.code === 'ENOENT' ? 'the package is incomplete; reinstall it (npm install -g pilots)' : result.error.message;
  process.stderr.write(`pilot: could not start ${found.binary}: ${why}\n`);
  process.exit(1);
}
if (result.signal) {
  process.kill(process.pid, result.signal);
  // A signal this process ignores or survives still has to end it non-zero.
  process.exit(1);
}
process.exit(result.status ?? 1);
