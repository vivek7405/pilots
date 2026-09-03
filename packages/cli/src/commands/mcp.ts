/**
 * `pilot mcp`: run the MCP server on stdio.
 *
 * The command does nothing but build a client and hand it over. Everything
 * interesting is in `src/mcp/`, and the import is dynamic so that the stdout
 * guard runs before the MCP SDK is loaded rather than after commander pulled
 * the whole tree in.
 */

import { Command } from 'commander'

import { clientFromEnv, type GlobalOptions } from '../config.ts'

export function createMcpCommand(): Command {
  return new Command('mcp')
    .description('run the pilots MCP server on stdio (for Claude Code, Claude Desktop, …)')
    .action(async function (this: Command) {
      const opts = this.optsWithGlobals() as GlobalOptions
      const client = clientFromEnv(opts)
      const { startMcpServer } = await import('../mcp/server.ts')
      await startMcpServer(client)
    })
}
