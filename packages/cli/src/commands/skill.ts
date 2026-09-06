/**
 * `pilot skill install`: make the skill available to every project at once.
 *
 * A symlink rather than a copy, so an upgrade of the CLI upgrades the skill.
 * A copy would leave a stale corpus behind describing tools that no longer
 * exist, which is worse than no skill at all.
 */

import { existsSync, lstatSync, mkdirSync, rmSync, symlinkSync } from 'node:fs'
import { homedir } from 'node:os'
import { join } from 'node:path'

import { Command } from 'commander'

import { CliError, isJSONMode, note, printJSON } from '../output.ts'
import { skillRoot } from '../mcp/skill.ts'

export function createSkillCommand(): Command {
  const skill = new Command('skill').description('manage the pilots agent skill')

  skill
    .command('install')
    .description('link the pilots skill into ~/.claude/skills')
    .action(function (this: Command) {
      // Resolved from the package, never from the working directory: this is
      // the global install, and a project's edited copy must not become
      // everyone's.
      const source = skillRoot(join(homedir(), 'no-such-directory'))
      if (!source) {
        throw new CliError('the pilots skill is not in this installation; reinstall @pilots/cli')
      }
      const target = join(homedir(), '.claude', 'skills', 'pilots')

      if (existsSync(target)) {
        const stat = lstatSync(target)
        if (!stat.isSymbolicLink()) {
          // A real directory is somebody's own copy or their edits. Replacing
          // it silently would delete work nobody asked us to touch.
          throw new CliError(
            `${target} is a directory, not a link; remove it first if you want the packaged skill`,
          )
        }
        rmSync(target)
      }
      mkdirSync(join(homedir(), '.claude', 'skills'), { recursive: true })
      symlinkSync(source, target, 'dir')

      if (isJSONMode()) {
        printJSON({ linked: target, to: source })
        return
      }
      note(`linked ${target} -> ${source}`)
    })

  return skill
}
