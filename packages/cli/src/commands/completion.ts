/**
 * `pilot completion <shell>`: the completion script, on stdout.
 *
 * Generated from commander's own command tree rather than written by hand, so
 * a command added in `main.ts` completes without anyone remembering to update
 * a second list. commander 15 has no built-in generator, hence this one.
 *
 * The script is the RESULT here, not a diagnostic, so it goes to stdout: the
 * whole point is `eval "$(pilot completion bash)"`.
 */

import type { Command } from 'commander'

import { CliError } from '../output.ts'

export const SHELLS = ['bash', 'zsh', 'fish'] as const

export type Shell = (typeof SHELLS)[number]

interface Node {
  /** The words leading here, `pilot` excluded: `['machines', 'ls']`. */
  path: string[]
  name: string
  description: string
  subcommands: string[]
  options: string[]
}

/** Flattens the command tree, one node per command, parents before children. */
export function walk(program: Command): Node[] {
  const out: Node[] = []
  const visit = (cmd: Command, path: string[]): void => {
    const subcommands = cmd.commands.map((c) => c.name())
    out.push({
      path,
      name: cmd.name(),
      description: cmd.description(),
      subcommands,
      options: cmd.options.map((o) => o.long).filter((l): l is string => Boolean(l)),
    })
    for (const child of cmd.commands) {
      // Aliases complete too: `pilot service ls` is `pilot services ls`.
      for (const alias of [child.name(), ...child.aliases()]) {
        visit(child, [...path, alias])
      }
    }
  }
  visit(program, [])
  return out
}

export function generate(program: Command, shell: Shell): string {
  const nodes = walk(program)
  if (shell === 'bash') return bash(nodes)
  if (shell === 'zsh') return zsh(nodes)
  return fish(nodes)
}

/**
 * Bash walks COMP_WORDS to the deepest command it recognises and offers that
 * command's subcommands and long options. One case per path, so the offer is
 * exact rather than a union of every command's flags.
 */
function bash(nodes: Node[]): string {
  const cases = nodes
    .map((n) => {
      const key = n.path.join(' ')
      const words = [...n.subcommands, ...n.options].join(' ')
      return `    ${JSON.stringify(key)}) __pilot_words=${JSON.stringify(words)} ;;`
    })
    .join('\n')
  return `_pilot() {
  local cur path i word __pilot_words
  cur="\${COMP_WORDS[COMP_CWORD]}"
  path=""
  for ((i = 1; i < COMP_CWORD; i++)); do
    word="\${COMP_WORDS[i]}"
    case "$word" in -*) continue ;; esac
    if [ -z "$path" ]; then path="$word"; else path="$path $word"; fi
  done
  case "$path" in
${cases}
    *) __pilot_words="" ;;
  esac
  COMPREPLY=($(compgen -W "$__pilot_words" -- "$cur"))
}
complete -F _pilot pilot
`
}

/**
 * zsh, as one `_arguments` per level reached through the same path walk. Kept
 * deliberately close to the bash shape: two generators that diverge in
 * structure are two things to debug.
 */
function zsh(nodes: Node[]): string {
  const cases = nodes
    .map((n) => {
      const key = n.path.join(' ')
      const words = [...n.subcommands, ...n.options].join(' ')
      return `    ${JSON.stringify(key)}) __pilot_words=${JSON.stringify(words)} ;;`
    })
    .join('\n')
  // The result variable is __pilot_words, not words: `words` is the array zsh
  // fills with the command line, and a `local words` here would blank the one
  // thing the walk below has to read.
  return `#compdef pilot
_pilot() {
  local path i word __pilot_words
  path=""
  for ((i = 2; i < CURRENT; i++)); do
    word="\${words[i]}"
    case "$word" in -*) continue ;; esac
    if [ -z "$path" ]; then path="$word"; else path="$path $word"; fi
  done
  case "$path" in
${cases}
    *) __pilot_words="" ;;
  esac
  _arguments "*: :(\${=__pilot_words})"
}
compdef _pilot pilot
`
}

/**
 * fish completes by condition rather than by walking words itself, so each
 * command gets a guard naming the path that has to be on the line already.
 */
function fish(nodes: Node[]): string {
  const lines: string[] = [
    'function __fish_pilot_path',
    '  set -l parts',
    '  for word in (commandline -opc)[2..-1]',
    '    string match -q -- "-*" $word; and continue',
    '    set parts $parts $word',
    '  end',
    '  string join " " $parts',
    'end',
    '',
  ]
  for (const node of nodes) {
    // Quoted: at the root the path is empty, and an UNQUOTED command
    // substitution that prints nothing expands to zero arguments, leaving
    // fish to evaluate `test = ''` -- not a valid test, so `pilot <TAB>`
    // would complete nothing at all.
    const guard = `test "(__fish_pilot_path)" = ${quoteFish(node.path.join(' '))}`
    for (const sub of node.subcommands) {
      const child = nodes.find((n) => n.path.join(' ') === [...node.path, sub].join(' '))
      const description = child?.description ?? ''
      lines.push(
        `complete -c pilot -f -n ${quoteFish(guard)} -a ${quoteFish(sub)} -d ${quoteFish(description)}`,
      )
    }
    for (const option of node.options) {
      lines.push(`complete -c pilot -f -n ${quoteFish(guard)} -l ${quoteFish(option.replace(/^--/, ''))}`)
    }
  }
  return lines.join('\n') + '\n'
}

function quoteFish(text: string): string {
  return `'${text.replace(/\\/g, '\\\\').replace(/'/g, "\\'")}'`
}

export function createCompletionCommand(program: Command): Command {
  // The program is a parameter rather than an import of buildProgram, which
  // would be a cycle: main.ts is what builds this command.
  return program
    .createCommand('completion')
    .argument('<shell>', `the shell to generate for: ${SHELLS.join(', ')}`)
    .description('print a shell completion script; eval it from your shell rc')
    .action(function (shell: string) {
      if (!(SHELLS as readonly string[]).includes(shell)) {
        throw new CliError(`unknown shell ${shell}`, { hint: `pilot completion ${SHELLS.join('|')}` })
      }
      process.stdout.write(generate(program, shell as Shell))
    })
}
