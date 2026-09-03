#!/usr/bin/env node
/**
 * The `pilot` entry point.
 *
 * Deliberately plain JavaScript with no imports above the version check: the
 * CLI's own source is TypeScript run by Node's type stripping, and a Node
 * older than the floor fails on the *import* with a stack trace that says
 * nothing about the version. Checking first turns that into one sentence.
 * `webjs/packages/cli/bin/webjs.js` does the same, for the same reason.
 */

const MIN_MAJOR = 24
const major = Number(process.versions.node.split('.')[0])

if (!Number.isFinite(major) || major < MIN_MAJOR) {
  process.stderr.write(
    `pilot needs Node ${MIN_MAJOR} or newer (running ${process.versions.node}).\n` +
      'The CLI runs TypeScript directly through the runtime type stripper,\n' +
      'which is on by default from Node 23.6. Upgrade Node and retry.\n',
  )
  process.exit(1)
}

await import('../src/main.ts')
