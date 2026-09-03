/**
 * `pilot`: the command tree.
 *
 * One `createXCommand()` per file under `commands/`, assembled here. The
 * program is exported rather than only executed so the tests can drive the
 * real argument parsing instead of a re-implementation of it.
 */

import { Command } from 'commander'

import { VERSION } from './version.ts'

export function buildProgram(): Command {
  const program = new Command()

  program
    .name('pilot')
    .description('sandboxes and services on one primitive')
    .version(VERSION, '-v, --version', 'print the CLI version')
    .option('--json', 'print the API response as JSON on stdout, errors on stderr')
    .option('--api-url <url>', 'the fleet to talk to; wins over PILOT_API_URL and the credentials file')
    .showHelpAfterError()

  return program
}

// `bin/pilot.js` imports this module for its side effect. Guarding on argv
// rather than on `import.meta.main` keeps the module importable by the tests.
if (process.argv[1]?.endsWith('pilot.js') || process.env.PILOT_CLI_RUN === '1') {
  await buildProgram().parseAsync(process.argv)
}
