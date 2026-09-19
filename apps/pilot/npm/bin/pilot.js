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
 *
 * spawn, not spawnSync: a blocked event loop cannot hear a signal, so a
 * SIGTERM sent to this pid (timeout(1), a supervisor, a cancelled CI step)
 * killed the launcher and left the binary running with nobody above it.
 */
const { spawn } = require('node:child_process');
const fs = require('node:fs');
const { binaryFor } = require('../lib/resolve.js');

const found = binaryFor(process.platform, process.arch);
if ('error' in found) {
  process.stderr.write(found.error + '\n');
  process.exit(1);
}

/** Sent to this pid alone, so the child has not seen them: passed on. */
const FORWARDED = ['SIGTERM', 'SIGHUP'];
/**
 * Sent by the terminal to the whole foreground group, so the child already
 * has its own copy. Passing it on would deliver it twice, and a second ^C is
 * what ends a Go program's graceful shutdown. This process only has to not
 * die first, and then end the way the child does.
 */
const SHARED = ['SIGINT', 'SIGQUIT'];

function fail(error) {
  const why = error.code === 'ENOENT' ? 'the package is incomplete; reinstall it (npm install -g pilots)' : error.message;
  process.stderr.write(`pilot: could not start ${found.binary}: ${why}\n`);
  process.exit(1);
}

function launch(repaired) {
  const child = spawn(found.binary, process.argv.slice(2), { stdio: 'inherit' });
  const handlers = new Map();
  for (const sig of FORWARDED) handlers.set(sig, () => child.kill(sig));
  for (const sig of SHARED) handlers.set(sig, () => {});
  for (const [sig, handler] of handlers) process.on(sig, handler);
  const release = () => {
    for (const [sig, handler] of handlers) process.removeListener(sig, handler);
  };

  child.on('error', (error) => {
    release();
    if (error.code === 'EACCES' && !repaired) {
      // A tarball extracted by a tool that drops the mode bits. Worth one
      // repair rather than an error that sends someone to chmod inside
      // node_modules.
      try {
        fs.chmodSync(found.binary, 0o755);
        return launch(true);
      } catch {
        // Fall through to the error below with the original cause.
      }
    }
    fail(error);
  });
  child.on('exit', (code, signal) => {
    release();
    if (signal) {
      process.kill(process.pid, signal);
      // A signal this process ignores or survives still has to end it non-zero.
      process.exit(1);
    }
    process.exit(code ?? 1);
  });
}

launch(false);
