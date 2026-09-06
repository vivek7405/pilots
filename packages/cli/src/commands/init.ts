/**
 * `pilot init`: make a repository ready for an agent to deploy it.
 *
 * Three writes, all idempotent, none destructive:
 *
 *   1. the skill into `.agents/skills/pilots/`, which is what makes the
 *      references readable off disk by an agent that is not driving the MCP;
 *   2. an entry for `pilot mcp` in the MCP client configs, ADDED beside
 *      whatever else is there, never replacing the file;
 *   3. a stanza in `AGENTS.md` pointing at the skill.
 *
 * Idempotent because it will be run again. A second run that duplicated the
 * stanza or clobbered another server's config entry would make `init` a thing
 * people are afraid to run twice, which is the same as a thing they run once
 * and then never again when it matters.
 */

import { cpSync, existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'

import { Command } from 'commander'

import { CliError, isJSONMode, note, printJSON } from '../output.ts'
import { skillRoot } from '../mcp/skill.ts'

/** The stanza appended to AGENTS.md, matched by its first line on a re-run. */
const AGENTS_HEADING = '## Deploying with pilots'

const AGENTS_STANZA = `${AGENTS_HEADING}

This repository deploys to pilots. The skill is at \`.agents/skills/pilots/\`;
read \`SKILL.md\` before deploying, and load at most two reference pages.

The one call is \`pilot deploy\` in the app's directory, or the MCP \`deploy\`
tool with \`dir\`. The platform decides what the directory is. Do not write a
Dockerfile or a compose file first; write one only when the answer is
\`unknown_framework\`, and obey the two rules its \`details.rules\` names.

Every error carries \`code\`, \`next\` and \`details\`. Read \`next\` and do that.
`

const MCP_ENTRY = { type: 'stdio', command: 'pilot', args: ['mcp'] }

export function createInitCommand(): Command {
  return new Command('init')
    .argument('[dir]', 'the repository to set up', '.')
    .description('copy the pilots skill into a repository and register the MCP server')
    .action(function (this: Command, dirArg: string) {
      const dir = resolve(dirArg)
      const source = skillRoot(dir)
      if (!source) {
        throw new CliError(
          'the pilots skill is not in this installation; reinstall @pilots/cli',
        )
      }

      const done: string[] = []
      const skipped: string[] = []
      const record = (what: string, changed: boolean) => (changed ? done : skipped).push(what)

      const target = join(dir, '.agents', 'skills', 'pilots')
      record('.agents/skills/pilots', copySkill(source, target))
      record('.claude.json', mergeMCPConfig(join(dir, '.claude.json')))
      record('.cursor/mcp.json', mergeMCPConfig(join(dir, '.cursor', 'mcp.json')))
      record('AGENTS.md', appendStanza(join(dir, 'AGENTS.md')))

      if (isJSONMode()) {
        printJSON({ dir, written: done, already_present: skipped })
        return
      }
      for (const what of done) note(`wrote ${what}`)
      for (const what of skipped) note(`${what} is already set up`)
    })
}

/**
 * Copies the skill, unless the target already holds one.
 *
 * Never overwritten: a team that edited a page meant to, and `init` is not the
 * place to discover that. Removing the directory is how you get a fresh copy.
 */
function copySkill(source: string, target: string): boolean {
  if (existsSync(join(target, 'SKILL.md'))) return false
  mkdirSync(dirname(target), { recursive: true })
  cpSync(source, target, { recursive: true })
  return true
}

/**
 * Adds the pilots server to an MCP client config, keeping everything else.
 *
 * The file belongs to the user and usually names other servers. Rewriting it
 * would take those away, so this reads what is there, adds one key, and writes
 * it back. A file that does not parse is left alone and reported, because
 * guessing at a broken config is worse than saying so.
 */
function mergeMCPConfig(path: string): boolean {
  let config: Record<string, unknown> = {}
  if (existsSync(path)) {
    try {
      config = JSON.parse(readFileSync(path, 'utf8')) as Record<string, unknown>
    } catch {
      throw new CliError(`${path} is not valid JSON; fix it or move it aside, then run pilot init again`)
    }
  }
  const servers = (config.mcpServers ?? {}) as Record<string, unknown>
  if (servers.pilots) return false
  servers.pilots = MCP_ENTRY
  config.mcpServers = servers
  mkdirSync(dirname(path), { recursive: true })
  writeFileSync(path, JSON.stringify(config, null, 2) + '\n')
  return true
}

/** Appends the stanza, unless its heading is already in the file. */
function appendStanza(path: string): boolean {
  const existing = existsSync(path) ? readFileSync(path, 'utf8') : ''
  if (existing.includes(AGENTS_HEADING)) return false
  const separator = existing === '' ? '' : existing.endsWith('\n\n') ? '' : existing.endsWith('\n') ? '\n' : '\n\n'
  writeFileSync(path, existing + separator + AGENTS_STANZA)
  return true
}
