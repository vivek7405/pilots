/**
 * `pilot add postgres`: edit the compose file in place, keeping its comments.
 *
 * The document is parsed with `yaml`'s `parseDocument` rather than `parse`,
 * because a round trip through plain objects would strip every comment in the
 * file. Somebody's note about why a service has two replicas is not this
 * command's to delete.
 */

import { chmodSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { basename, dirname, join } from 'node:path'

import { Command } from 'commander'
import { parseDocument, YAMLMap, YAMLSeq } from 'yaml'

import { loadCredentials, saveCredentials, type GlobalOptions } from '../config.ts'
import { CliError, isJSONMode, note, printJSON } from '../output.ts'
import { COMPOSE_NAMES, findComposeFile } from '../compose/find.ts'
import { databaseURL, generatePassword, postgresFragment } from '../compose/postgres.ts'

export function createAddCommand(): Command {
  const add = new Command('add').description('add a managed piece to the compose file')

  add
    .command('postgres')
    .description('add a Postgres service, a durability mode and a generated password')
    .option('--durable-volume', 'put the data directory on a volume (RPO 0, slower commits)', false)
    .option('--name <name>', 'the service name to add', 'postgres')
    .option('--app <name>', 'the app the generated secrets belong to')
    .option('--dir <path>', 'the directory holding the compose file', '.')
    .action(async function (this: Command) {
      const opts = this.optsWithGlobals() as GlobalOptions & {
        durableVolume: boolean
        name: string
        app?: string
        dir: string
      }
      const file = findComposeFile(opts.dir)
      if (!file) {
        throw new CliError(`no compose file in ${opts.dir}: looked for ${COMPOSE_NAMES.join(', ')}`)
      }
      const composeDir = dirname(file)
      const app = opts.app ?? basename(composeDir)

      const doc = parseDocument(readFileSync(file, 'utf8'))
      const services = doc.get('services')
      if (services instanceof YAMLMap && services.has(opts.name)) {
        throw new CliError(`service ${opts.name} already exists in ${file}`)
      }

      const fragment = postgresFragment({ durableVolume: opts.durableVolume })
      const node = doc.createNode(fragment.service) as YAMLMap
      // The mode is documentation the CLI reads back, so it says what it means
      // on the line where it is set rather than in a doc nobody opens.
      const xPilots = node.get('x-pilots') as YAMLMap | undefined
      const durable = xPilots?.items.find((i) => String(i.key) === 'durable_volume')
      if (durable) (durable.value as { comment?: string }).comment = fragment.modeComment
      // The healthcheck test reads as one command, so it is written as one
      // line. A block sequence here turns a two-element list into four lines
      // of scrollback in a file people edit by hand.
      const health = node.get('healthcheck') as YAMLMap | undefined
      const healthTest = health?.get('test')
      if (healthTest instanceof YAMLSeq) healthTest.flow = true
      doc.setIn(['services', opts.name], node)

      for (const [volume, value] of Object.entries(fragment.volumes)) {
        doc.setIn(['volumes', volume], doc.createNode(value))
      }

      writeFileSync(file, doc.toString())

      for (const [rel, content] of Object.entries(fragment.files)) {
        const path = join(composeDir, rel)
        mkdirSync(dirname(path), { recursive: true })
        writeFileSync(path, content)
        if (rel.endsWith('.sh')) chmodSync(path, 0o755)
      }

      const password = generatePassword()
      const url = databaseURL(password, opts.name)
      const creds = loadCredentials()
      if (creds) {
        saveCredentials({
          ...creds,
          secrets: {
            ...creds.secrets,
            [app]: { ...creds.secrets?.[app], postgres_password: password, database_url: url },
          },
        })
      }

      if (isJSONMode()) {
        printJSON({
          service: opts.name,
          mode: fragment.mode,
          app,
          secrets: { postgres_password: password, database_url: url },
        })
        return
      }

      // Printed once, to stderr, and never again by any command: the value is
      // stored sealed on the fleet and in plaintext only in the 0600
      // credentials file on this machine.
      note(
        `${opts.name} added to ${basename(file)} (mode: ${
          fragment.mode === 'wal-archive'
            ? 'local data dir + WAL archive'
            : 'data dir on a volume'
        }; see docs/postgres.md)`,
      )
      if (!creds) {
        note('not logged in: the values below are not stored, keep them somewhere safe')
      }
      note(`Add to the services that need it:   environment: { DATABASE_URL: secret://database_url }`)
      note('On another machine, export before deploying:')
      note(`  export PILOT_SECRET_POSTGRES_PASSWORD=${password}`)
      note(`  export PILOT_SECRET_DATABASE_URL=${url}`)
    })

  return add
}
