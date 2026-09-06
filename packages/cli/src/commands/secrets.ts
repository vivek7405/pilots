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
import { collect, parseKeyValues } from '../resolve.ts'
import { assertSecretName, importSecrets, listSecretNames, setSecret } from '../secrets/store.ts'

interface ScopeOptions {
  app?: string
  dir: string
  env?: string[]
  file?: string
}

export function createSecretsCommand(): Command {
  const secrets = new Command('secrets')
    .alias('secret')
    .description('the values secret:// references resolve to, on this machine')

  // The four flags `deploy` derives its app from, so the name a value is
  // stored under is the name the deploy looks it up under. Anything less and
  // `deploy --env COMPOSE_PROJECT_NAME=prod` resolves against an app nothing
  // ever wrote to.
  const scoped = (cmd: Command): Command =>
    cmd
      .option('--app <name>', 'the app the secrets belong to (default: the compose file\'s app)')
      .option('--dir <path>', 'the directory holding the compose file', '.')
      .option('--env <K=V>', 'add to the environment the app name is derived from (repeatable)', collect)
      .option('--file <path>', 'use this compose file instead of searching')

  scoped(secrets.command('set <name> [value]'))
    .description('store one secret; prompts for the value when it is omitted')
    // A secret that starts with a dash is a real shape (`sk-...` keys,
    // anything base64url, a PEM block), and the parser reads it as an option
    // and quotes the token back in its refusal, which puts the value in the
    // scrollback this command exists to keep it out of. Accepting an unknown
    // option makes it an operand, so the value stores instead of leaking.
    //
    // A mistyped flag is still refused: it becomes an operand too, and `set`
    // takes at most two, so `--ap x` trips the arity check. That message
    // counts arguments and never quotes one.
    .allowUnknownOption()
    .configureOutput({
      outputError: (str, write) => write(redactExcessArguments(str)),
    })
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

/**
 * `--app`, else the compose file's app.
 *
 * Every input `deploy` uses, resolved the way `deploy` resolves it: `--file`
 * relative to `--dir`, and `--env` merged over the directory's `.env`. The two
 * commands have to agree on the name or the value is stored where the deploy
 * will not look for it.
 */
function appFor(opts: ScopeOptions): string {
  if (opts.app) return opts.app
  const dir = resolve(opts.dir)
  const file = opts.file ? resolve(dir, opts.file) : findComposeFile(dir)
  if (!file) {
    throw new CliError(
      `no compose file in ${dir} to take the app from (looked for ${COMPOSE_NAMES.join(', ')}): pass --app`,
    )
  }
  return composeAppName(file, parseKeyValues(opts.env))
}

/**
 * Rewrites the arity refusal so it carries counts and not arguments.
 *
 * commander ends "too many arguments" with the argument list, and on `set` one
 * of those arguments is the secret, so the message is rebuilt from the two
 * numbers and nothing else. Only digits are taken from the original; a shape
 * this does not recognise is replaced wholesale rather than passed through,
 * because passing through is what prints the value.
 */
function redactExcessArguments(str: string): string {
  if (!str.includes('too many arguments')) return str
  const counts = /Expected (\d+) arguments? but got (\d+)/.exec(str)
  const got = counts ? ` Expected ${counts[1]} arguments but got ${counts[2]}.` : ''
  return (
    `error: too many arguments for 'set'.${got}\n` +
    '`set` takes a name and an optional value. An option it does not know is read\n' +
    'as one of them, so check the flag names. The arguments are not listed here\n' +
    'because one of them is the secret.\n'
  )
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
