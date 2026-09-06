/**
 * `pilot open <target>`: the service or machine, in a browser.
 *
 * The opener is spawned with an argv array and never through a shell. That is
 * not paranoia about this URL, it is the failure mode the `sprite` binary has:
 * re-executing through `$SHELL -c` hangs on every non-interactive invocation,
 * so nothing here goes near a shell.
 */

import { spawn } from 'node:child_process'
import { platform } from 'node:os'

import { Command } from 'commander'

import { clientFromEnv, type GlobalOptions } from '../config.ts'
import { CliError, isJSONMode, note, printJSON } from '../output.ts'
import { resolveTarget } from '../resolve.ts'

/** Injectable so a test can prove nothing was spawned. */
export type Spawn = (file: string, args: string[]) => void

/**
 * Fire and forget: no pipes, its own process group.
 *
 * `execFile` would give the child piped stdio, and `unref()` releases only the
 * process handle, never those pipes. `xdg-open` execs the browser, the browser
 * inherits fd 1 and 2, and node then waits on a pipe that stays open for as
 * long as the browser runs -- so `pilot open` would not return until the user
 * quit their browser. `stdio: 'ignore'` is what makes the command return.
 */
const defaultSpawn: Spawn = (file, args) => {
  const child = spawn(file, args, { detached: true, stdio: 'ignore', shell: false })
  // A missing opener emits `error`; with no listener that is an uncaught
  // exception rather than a URL the reader can still copy off the screen.
  child.on('error', (err) => note(`could not run ${file}: ${err.message}`))
  child.unref()
}

/** Injectable beside the spawn seam, so a test can read stdout without owning it. */
export type Emit = (line: string) => void

const defaultEmit: Emit = (line) => {
  process.stdout.write(line + '\n')
}

/** The platform's opener, as a binary and its arguments. */
export function openerFor(url: string, os: string = platform()): { file: string; args: string[] } {
  if (os === 'darwin') return { file: 'open', args: [url] }
  // The empty string is `start`'s title argument. Without it a URL in quotes
  // is read as the window title and nothing opens.
  if (os === 'win32') return { file: 'cmd', args: ['/c', 'start', '', url] }
  return { file: 'xdg-open', args: [url] }
}

export function createOpenCommand(spawn: Spawn = defaultSpawn, emit: Emit = defaultEmit): Command {
  return new Command('open')
    .argument('<target>', 'a service or machine, by id or name')
    .description('open a service or machine URL in a browser')
    .option('-p, --print', 'print the URL instead of opening it')
    .action(async function (this: Command, target: string) {
      const opts = this.optsWithGlobals() as GlobalOptions & { print?: boolean }
      const client = clientFromEnv(opts)
      const found = await resolveTarget(client, target)
      if (!found.url) {
        throw new CliError(`${found.name} has no URL`, {
          hint: `set x-pilots.domain in the compose file, or pilot domains add <host> --service ${found.name}`,
        })
      }

      if (isJSONMode()) {
        printJSON({ url: found.url, opened: false })
        return
      }
      // Nothing to open when stdout is a pipe: the caller wanted the URL.
      if (opts.print || !process.stdout.isTTY) {
        emit(found.url)
        return
      }
      const { file, args } = openerFor(found.url)
      spawn(file, args)
      note(found.url)
    })
}
