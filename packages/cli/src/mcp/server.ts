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

import { readFileSync } from 'node:fs'

import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js'
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js'
import type { PilotsClient } from '@pilots/sdk'
import { z } from 'zod'

import { VERSION } from '../version.ts'
import { skillPages } from './skill.ts'
import { registerTools } from './tools.ts'

export function buildMcpServer(client: PilotsClient): McpServer {
  const server = new McpServer({ name: 'pilots', version: VERSION })
  registerTools(server, client)
  registerSkill(server)
  registerPrompts(server)
  return server
}

/**
 * The skill, as `pilots-docs://` resources.
 *
 * The same files the `docs` tool reads and the same files `pilot init` copies
 * into a repository. A client that browses resources and one that calls tools
 * see one corpus, so a fix to a page cannot land in one surface and miss the
 * other.
 *
 * Missing entirely on a broken install rather than served empty: a resource
 * that answers with nothing looks like a page with nothing to say.
 */
function registerSkill(server: McpServer): void {
  for (const page of skillPages()) {
    const uri = `pilots-docs://${page.name}`
    server.registerResource(
      page.name,
      uri,
      {
        title: page.name,
        description: `The pilots skill: ${page.name}`,
        mimeType: 'text/markdown',
      },
      () => ({ contents: [{ uri, text: readFileSync(page.path, 'utf8') }] }),
    )
  }
}

/**
 * One prompt, for the one call.
 *
 * A client that surfaces prompts gives the user a "deploy this" button, and
 * what the button has to say is the same thing the skill says: call init, then
 * deploy with the directory, then follow `next`.
 */
function registerPrompts(server: McpServer): void {
  server.registerPrompt(
    'deploy',
    {
      title: 'Deploy a directory to a URL',
      description: 'Take a directory to a URL in one call, following the platform\'s next step on any error.',
      argsSchema: { dir: z.string().describe('the directory to deploy') },
    },
    ({ dir }) => ({
      messages: [
        {
          role: 'user' as const,
          content: {
            type: 'text' as const,
            text:
              `Deploy ${dir} to pilots. Call init first, then deploy with dir=${dir}. ` +
              'On any error, read code, next and details, and do what next says. ' +
              'Report the URL when it is serving.',
          },
        },
      ],
    }),
  )
}

export async function startMcpServer(client: PilotsClient): Promise<void> {
  const server = buildMcpServer(client)
  await server.connect(new StdioServerTransport())
}
