// The guard is first on purpose; see quiet.ts. Nothing may be imported above it.
import './quiet.ts'

/**
 * `pilot mcp`: the agent's front door, over stdio.
 *
 * stdio rather than a remote HTTP transport because it is what every desktop
 * MCP client launches and it needs no OAuth server to stand in front of it.
 * The credential is the same API key every other client uses, taken from the
 * environment or the credentials file.
 */

import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js'
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js'
import type { PilotsClient } from '@pilots/sdk'

import { VERSION } from '../version.ts'
import { registerTools } from './tools.ts'

export function buildMcpServer(client: PilotsClient): McpServer {
  const server = new McpServer({ name: 'pilots', version: VERSION })
  registerTools(server, client)
  return server
}

export async function startMcpServer(client: PilotsClient): Promise<void> {
  const server = buildMcpServer(client)
  await server.connect(new StdioServerTransport())
}
