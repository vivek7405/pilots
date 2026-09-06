/**
 * `pilot console <machine>`: a shell on a machine, on a real terminal.
 *
 * It is `machines exec` in terminal mode and nothing more. The same route, the
 * same frames, the same exit verdict: the server grew a `tty` flag on the exec
 * stream rather than a second protocol, so there is one thing to route,
 * authenticate and proxy, and this command opens no socket of its own.
 *
 * Two behaviours are the whole reason it is a separate command:
 *
 *   - the local terminal goes into RAW MODE, so ctrl-c, tab completion and a
 *     full-screen editor reach the remote shell as bytes instead of being
 *     eaten by the local line discipline. That also means a crash which skips
 *     the restore leaves the user with a terminal that no longer echoes, which
 *     is why every exit path here puts it back;
 *   - a window change is forwarded, so `stty size` in the guest keeps agreeing
 *     with the window the user is actually looking at.
 *
 * A session is bound to its socket: closing it kills the shell. There is no
 * detach and reattach, and reaching for one by leaving a socket open would
 * leak a shell, a PTY and two goroutines per abandoned window inside a guest
 * whose whole job is to go idle.
 */

import { Command } from 'commander'

import { clientFromEnv, type GlobalOptions } from '../config.ts'
import { CliError, isJSONMode, note } from '../output.ts'
import { collect, resolveMachine } from '../resolve.ts'
import { execStream, processTerminal, type Terminal } from './machines.ts'

/**
 * A login shell, so the guest's own profile has run before the first prompt.
 *
 * Overridable by anything after `--`, which is how `pilot console box -- bash`
 * or a REPL gets one.
 */
const DEFAULT_ARGV = ['/bin/sh', '-l']

export function createConsoleCommand(term: Terminal = processTerminal): Command {
  return new Command('console')
    .argument('<machine>', 'a machine, by id or name')
    .description('an interactive shell on a machine; everything after -- replaces /bin/sh -l')
    .option('--cwd <dir>', 'working directory in the guest')
    .option('--env <K=V>', 'an environment variable (repeatable)', collect)
    .option('--user <name>', 'run as this user')
    .allowExcessArguments(true)
    .action(async function (this: Command, machine: string) {
      const opts = this.optsWithGlobals() as GlobalOptions & Record<string, unknown>

      // Both refusals come before the fleet is touched. A command that cannot
      // work should say so without a round trip, and without waking a
      // suspended sandbox to tell it.
      //
      // `--json` is checked first because it is the flag the caller typed. A
      // shell session is a stream of terminal bytes with no document at the
      // end of it, so there is no JSON this command could ever print, and
      // half-honouring the flag by printing nothing on stdout would leave a
      // program parsing an empty string as if it were a result.
      if (isJSONMode()) {
        throw new CliError('console is interactive, so it has no --json form', {
          hint: 'for output a program can parse, run pilot machines exec <machine> --json -- <argv...>',
        })
      }
      // Raw mode needs a terminal to put into it, and the initial window comes
      // off stdout. Neither has an answer on a pipe, and a console that
      // half-works off one is worse than one that names the command that does:
      // it connects, draws nothing, and takes the shell down with it on the
      // first stray byte.
      const missing = notATerminal(term)
      if (missing) {
        throw new CliError(`console needs a terminal, and ${missing} is not one`, {
          hint: 'from a script, a pipe or an agent, run pilot machines exec <machine> -- <argv...>',
        })
      }

      const client = clientFromEnv(opts)
      const found = await resolveMachine(client, machine)
      const argv = this.args.slice(1)
      // stderr, never stdout: the session's own bytes are the output here, and
      // a banner mixed into them is a banner in the middle of a `tmux` screen.
      note(`connected to ${found.name} (${found.id}); the shell ends when you leave it`)
      // The remote status, exactly as `machines exec` reports it. Leaving a
      // shell with `exit 3` exits 3.
      process.exitCode = await execStream(
        client,
        found.id,
        argv.length > 0 ? argv : DEFAULT_ARGV,
        { ...opts, tty: true },
        term,
      )
    })
}

/** Which of the two streams is not a terminal, if either. */
function notATerminal(term: Terminal): string | null {
  if (!term.stdin.isTTY) return 'stdin'
  if (!term.stdout.isTTY) return 'stdout'
  return null
}
