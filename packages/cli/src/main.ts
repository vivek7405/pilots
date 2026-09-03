/**
 * `pilot`: the command tree.
 *
 * One `createXCommand()` per file under `commands/`, assembled here. The
 * program is exported rather than only executed so the tests can drive the
 * real argument parsing instead of a re-implementation of it.
 *
 * Every action throws rather than exiting: the one `fail()` here is what turns
 * an error into the documented rendering and exit 1, so a new command cannot
 * accidentally invent its own error format.
 */

import { Command } from 'commander'

import { createDeployCommand } from './commands/deploy.ts'
import { createDomainsCommand } from './commands/domains.ts'
import { createLoginCommand, createLogoutCommand, createWhoamiCommand } from './commands/login.ts'
import { createMachinesCommand } from './commands/machines.ts'
import { createPromoteCommand } from './commands/promote.ts'
import { createServicesCommand } from './commands/services.ts'
import { createStatusCommand } from './commands/status.ts'
import { createVolumesCommand } from './commands/volumes.ts'
import { fail, setJSONMode } from './output.ts'
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
    .hook('preAction', (thisCommand) => {
      setJSONMode(Boolean(thisCommand.opts().json))
    })

  program.addCommand(createLoginCommand())
  program.addCommand(createLogoutCommand())
  program.addCommand(createWhoamiCommand())
  program.addCommand(createMachinesCommand())
  program.addCommand(createServicesCommand())
  program.addCommand(createDomainsCommand())
  program.addCommand(createVolumesCommand())
  program.addCommand(createPromoteCommand())
  program.addCommand(createStatusCommand())
  program.addCommand(createDeployCommand())

  return program
}

export async function run(argv: string[] = process.argv): Promise<void> {
  // The --json flag has to be known before the first error can be rendered,
  // and an error can happen during parsing itself. Reading argv directly is
  // the only thing available that early.
  setJSONMode(argv.includes('--json'))
  try {
    await buildProgram().parseAsync(argv)
  } catch (err) {
    fail(err)
  }
}

// `bin/pilot.js` imports this module for its side effect. Guarding on argv
// rather than running unconditionally keeps the module importable by tests.
if (process.argv[1]?.endsWith('pilot.js') || process.env.PILOT_CLI_RUN === '1') {
  await run()
}
