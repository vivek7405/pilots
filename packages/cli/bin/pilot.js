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

// Node announces its own type stripper with an ExperimentalWarning on some
// releases in the 22 and 24 lines. It fires on the import below, so it can be
// filtered here -- and it has to be, because this CLI makes promises about
// stderr: `--json` says stderr carries the server's error body and nothing
// else, and `pilot mcp` routes every diagnostic there. A note about the
// mechanism by which the CLI runs is not the caller's business. Every OTHER
// warning is re-emitted to the listeners Node installed.
const defaultWarningListeners = process.listeners('warning')
process.removeAllListeners('warning')
process.on('warning', (warning) => {
  if (warning.name === 'ExperimentalWarning' && /type strip/i.test(warning.message)) return
  for (const listener of defaultWarningListeners) listener.call(process, warning)
})

// The reader of stdout can go away first: `pilot machines ls | head -1`, a
// `grep -q` that has seen enough, a `less` the operator quit. Node disables
// the default SIGPIPE disposition at startup and reports the failed write as
// an `error` event on the stream instead, so with no listener that is an
// unhandled event -- a stack trace on stderr and exit 1. Both halves break a
// promise made a few lines up: stderr carries the server's body or a CLI
// diagnostic and nothing else, and exit 1 means the fleet refused, not that
// nobody was listening.
//
// 141 is 128 + SIGPIPE, the code a shell already reports for the left side of
// `seq 1 100000 | head -1`. Exiting rather than swallowing the error is what
// ends a `pilot logs --follow` whose reader is gone, instead of leaving it
// streaming from the fleet into a closed pipe. Every other stream error is
// rethrown and keeps today's behaviour.
process.stdout.on('error', (err) => {
  if (err.code === 'EPIPE') process.exit(141)
  throw err
})

// Calling `run()` explicitly, rather than importing this module for a
// side effect it decides to perform, is what makes the CLI work under the
// name it is installed as. `npm install -g` links `<prefix>/bin/pilot` to
// this file, and Node does not resolve argv[1] through that symlink: a
// module that sniffed `process.argv[1]` for `pilot.js` saw `/usr/bin/pilot`
// and silently did nothing, exit 0. The entry point is the one place that
// knows it is an entry point, so the decision belongs here.
const { run } = await import('../src/main.ts')
await run()
