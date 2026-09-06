/**
 * `pilot secrets` writes the values `secret://` references resolve to.
 *
 * Every subcommand is local file manipulation. No network and no fleet: a
 * value has to be storable on a laptop with no host reachable, and the file
 * it goes into is the same 0600 credentials file `pilot login` writes.
 *
 * Nothing here prints a value, on success or on failure. `ls` prints names
 * and a digest; `set` and `import` say what they stored, by name.
 */

import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

import { Command } from 'commander'

import { COMPOSE_NAMES, composeAppName, findComposeFile } from '../compose/find.ts'
import { envVarFor } from '../compose/secrets.ts'
import { credentialsPath, type GlobalOptions } from '../config.ts'
import { CliError, isJSONMode, note, printJSON, printTable } from '../output.ts'
import { promptSecret } from '../prompt.ts'
import { assertSecretName, importSecrets, listSecretNames, setSecret } from '../secrets/store.ts'

interface ScopeOptions {
  app?: string
  dir: string
}

export function createSecretsCommand(): Command {
  const secrets = new Command('secrets')
    .alias('secret')
    .description('the values secret:// references resolve to, on this machine')

  const scoped = (cmd: Command): Command =>
    cmd
      .option('--app <name>', 'the app the secrets belong to (default: the compose file\'s app)')
      .option('--dir <path>', 'the directory holding the compose file', '.')

  scoped(secrets.command('set <name> [value]'))
    .description('store one secret; prompts for the value when it is omitted')
    .action(async function (this: Command, name: string, value: string | undefined) {
      const opts = this.optsWithGlobals() as GlobalOptions & ScopeOptions
      const app = appFor(opts)
      // Before the value: a name the store would refuse is worth refusing
      // while the secret is still in the user's clipboard rather than after
      // they have typed it at the prompt.
      assertSecretName(name)
      const stored = value ?? (await readValue(name, app))
      setSecret(app, name, stored)
      if (isJSONMode()) printJSON({ app, name })
      else note(`stored ${name} for ${app} in ${credentialsPath()}`)
    })

  scoped(secrets.command('import <file>'))
    .description('store every KEY=value in a .env file (- reads stdin)')
    .action(function (this: Command, file: string) {
      const opts = this.optsWithGlobals() as GlobalOptions & ScopeOptions
      const app = appFor(opts)
      const names = importSecrets(app, file === '-' ? '-' : resolve(file))
      if (isJSONMode()) printJSON({ app, names })
      else note(`stored ${names.length} ${names.length === 1 ? 'secret' : 'secrets'} for ${app}: ${names.join(', ')}`)
    })

  scoped(secrets.command('ls').alias('list'))
    .description('list secret names and digests for an app; never values')
    .action(function (this: Command) {
      const opts = this.optsWithGlobals() as GlobalOptions & ScopeOptions
      const app = appFor(opts)
      const list = listSecretNames(app)
      if (isJSONMode()) printJSON({ app, secrets: list })
      else printTable([['NAME', 'DIGEST'], ...list.map((s) => [s.name, s.digest])])
    })

  return secrets
}

/** `--app`, else the compose file's app: the same name `pilot deploy` resolves against. */
function appFor(opts: ScopeOptions): string {
  if (opts.app) return opts.app
  const file = findComposeFile(resolve(opts.dir))
  if (!file) {
    throw new CliError(
      `no compose file in ${resolve(opts.dir)} to take the app from (looked for ${COMPOSE_NAMES.join(', ')}): pass --app`,
    )
  }
  return composeAppName(file)
}

/**
 * The value, from a prompt on a terminal or from stdin in a script.
 *
 * A trailing newline is stripped once, because `echo` adds one and a shell
 * heredoc adds one, and neither is part of the secret. Nothing else is
 * trimmed: a value that ends in a space ends in a space.
 */
async function readValue(name: string, app: string): Promise<string> {
  const hint = `pass the value as the second argument, pipe it on stdin, or export ${envVarFor(name)}`
  if (process.stdin.isTTY) return promptSecret(`Value for ${name} (${app}): `, hint)
  const piped = readFileSync(0, 'utf8').replace(/\r?\n$/, '')
  if (piped === '') throw new CliError(`no value on stdin for ${name}; ${hint}`)
  return piped
}
