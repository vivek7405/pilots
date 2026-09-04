/**
 * The thirteen tools.
 *
 * Two rules run through all of them. First, a result is JSON text, so an agent
 * parses it rather than reads it. Second, an error carries the SERVER'S body
 * verbatim: a 429 reaches the agent exactly as hostd wrote it, and a failed
 * build carries every NDJSON line, because reading the failing step and
 * patching the Dockerfile is the loop the structured log exists for.
 *
 * A non-zero exit from `exec` is NOT a tool error. The command ran; what its
 * status means is the agent's call, and marking it an error would make every
 * `grep` that found nothing look like a broken tool.
 */

import type { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js'
import { z } from 'zod'

import {
  BuildFailedError,
  PilotsError,
  type CreateServiceRequest,
  type HealthCheck,
  type PilotsClient,
  type Service,
  type UpdateServiceRequest,
} from '@pilots/sdk'

import { resolveMachine } from '../resolve.ts'
import { tarDirectory } from '../tar.ts'
import { generateDockerfile } from './dockerfile.ts'

interface ToolResult {
  content: { type: 'text'; text: string }[]
  isError?: boolean
  [key: string]: unknown
}

const DOCKERFILE_RULES =
  'Any Dockerfile you write must bind 0.0.0.0 (never 127.0.0.1, which serves only the guest itself) ' +
  'and read the port from $PORT. Both mistakes produce a build that succeeds and a URL that answers 502.'

const machineArg = z.string().describe('a machine id or name')

export function registerTools(server: McpServer, client: PilotsClient): void {
  server.registerTool(
    'create_machine',
    {
      title: 'Create a machine',
      description:
        'Create a microVM. A create is a restore from a template rather than a boot, so it is fast. ' +
        'The same primitive serves both a throwaway sandbox and a production replica; only the lifecycle knobs differ.',
      inputSchema: {
        name: z.string().optional().describe('a stable name; the URL is derived from it and never changes'),
        image: z.string().optional().describe('a rootfs build id from the build tool'),
        template: z.string().optional(),
        checkpoint: z.string().optional().describe('restore this checkpoint into the new machine'),
        vcpus: z.number().int().optional(),
        mem_mib: z.number().int().optional(),
        app: z.string().optional(),
        cmd: z.string().optional().describe('the start command, overriding the image'),
        env: z.record(z.string(), z.string()).optional(),
      },
    },
    (args) => wrap(() => client.machines.create(args)),
  )

  server.registerTool(
    'list_machines',
    {
      title: 'List machines',
      description: 'Every machine this API key can see, optionally narrowed to one app.',
      inputSchema: { app: z.string().optional() },
    },
    (args) =>
      wrap(async () => {
        const all = await client.machines.list()
        return args.app ? all.filter((m) => m.app === args.app) : all
      }),
  )

  server.registerTool(
    'status',
    {
      title: 'Status',
      description:
        'With a machine, that machine. Without one, the fleet: every host as the answering host sees it, ' +
        'plus a count of machines by state.',
      inputSchema: { machine: machineArg.optional() },
    },
    (args) =>
      wrap(async () => {
        if (args.machine) return await resolveMachine(client, args.machine)
        const [hosts, machines] = await Promise.all([client.hosts.list(), client.machines.list()])
        const byState: Record<string, number> = {}
        for (const machine of machines) byState[machine.state] = (byState[machine.state] ?? 0) + 1
        return { hosts, machines_by_state: byState, machines_total: machines.length }
      }),
  )

  server.registerTool(
    'exec',
    {
      title: 'Run a command',
      description:
        'Run a command and wait for it, returning stdout, stderr and the exit code. ' +
        'A non-zero exit is a result, not a tool error: decide what it means yourself. ' +
        'For output too large to hold in memory, use exec_stream.',
      inputSchema: {
        machine: machineArg,
        cmd: z.string().describe('the command line, run through a shell in the guest'),
        cwd: z.string().optional(),
        env: z.record(z.string(), z.string()).optional(),
        user: z.string().optional(),
        timeout_ms: z.number().int().optional(),
      },
    },
    (args) =>
      wrap(async () => {
        const machine = await resolveMachine(client, args.machine)
        return await client.machines.exec(machine.id, {
          cmd: args.cmd,
          ...(args.cwd ? { cwd: args.cwd } : {}),
          ...(args.env ? { env: args.env } : {}),
          ...(args.user ? { user: args.user } : {}),
          ...(args.timeout_ms ? { timeout_ms: args.timeout_ms } : {}),
        })
      }),
  )

  server.registerTool(
    'exec_stream',
    {
      title: 'Run a command over a stream',
      description:
        'Run a command over the streaming exec, collecting stdout and stderr. ' +
        'stdin is off unless you ask for it: a process holding an open stdin it never reads hangs.',
      inputSchema: {
        machine: machineArg,
        cmd: z.string(),
        cwd: z.string().optional(),
        env: z.record(z.string(), z.string()).optional(),
        user: z.string().optional(),
        stdin: z.boolean().default(false),
      },
    },
    (args) =>
      wrap(async () => {
        const machine = await resolveMachine(client, args.machine)
        const stream = client.machines.execStream(machine.id, ['sh', '-c', args.cmd], {
          ...(args.cwd ? { cwd: args.cwd } : {}),
          ...(args.env ? { env: args.env } : {}),
          ...(args.user ? { user: args.user } : {}),
          stdin: args.stdin ?? false,
        })
        const out: Buffer[] = []
        const err: Buffer[] = []
        stream.stdout.on('data', (chunk: Buffer) => out.push(chunk))
        stream.stderr.on('data', (chunk: Buffer) => err.push(chunk))
        const exitCode = await stream.wait()
        return {
          stdout: Buffer.concat(out).toString('utf8'),
          stderr: Buffer.concat(err).toString('utf8'),
          exit_code: exitCode,
        }
      }),
  )

  server.registerTool(
    'logs',
    {
      title: 'Console log',
      description: "The machine's console log, optionally only the last N lines.",
      inputSchema: { machine: machineArg, tail: z.number().int().positive().optional() },
    },
    (args) =>
      wrap(async () => {
        const machine = await resolveMachine(client, args.machine)
        const text = await client.machines.logs(machine.id)
        if (!args.tail) return { logs: text }
        const lines = text.split('\n')
        return { logs: lines.slice(Math.max(0, lines.length - args.tail)).join('\n') }
      }),
  )

  server.registerTool(
    'checkpoint',
    {
      title: 'Checkpoint a machine',
      description:
        'Capture the machine, memory included, so it can be restored to this exact moment. ' +
        'The resume gap does not grow with the machine size.',
      inputSchema: { machine: machineArg, comment: z.string().optional() },
    },
    (args) =>
      wrap(async () => {
        const machine = await resolveMachine(client, args.machine)
        return await client.machines.checkpoint(machine.id, args.comment ? { comment: args.comment } : {})
      }),
  )

  server.registerTool(
    'restore',
    {
      title: 'Restore a checkpoint',
      description:
        'Restore a checkpoint IN PLACE. The machine keeps its id, its URL and its agent token; ' +
        'nothing new is created, so every link to it still works.',
      inputSchema: { checkpoint: z.string().describe('a checkpoint id') },
    },
    (args) => wrap(() => client.checkpoints.restore(args.checkpoint)),
  )

  server.registerTool(
    'build',
    {
      title: 'Build a rootfs',
      description:
        'Tar a directory and build it into a bootable root filesystem, returning a rootfs build id for deploy. ' +
        'Pass `dockerfile` to build with a Dockerfile you wrote here, without writing it to disk first. ' +
        'On failure the result carries EVERY log line as NDJSON: read the failing step, fix the Dockerfile, call this again. ' +
        DOCKERFILE_RULES,
      inputSchema: {
        dir: z.string().describe('the build context directory'),
        dockerfile: z
          .string()
          .optional()
          .describe('Dockerfile CONTENTS (not a path); replaces any Dockerfile in the directory'),
      },
    },
    (args) =>
      wrap(async () => {
        const tar = tarDirectory(args.dir, args.dockerfile ? { extraFiles: { Dockerfile: args.dockerfile } } : {})
        const stream = await client.builds.create(new Uint8Array(tar))
        const rootfs = await stream.result()
        return { rootfs_build_id: rootfs, build_id: stream.buildId, steps: stream.lines.length }
      }),
  )

  server.registerTool(
    'deploy',
    {
      title: 'Deploy a service',
      description:
        'Create or update one service from a rootfs build id and wait for the new release to become current. ' +
        'This is the single-service primitive; a whole compose file is `pilot deploy` on the command line.',
      inputSchema: {
        name: z.string(),
        build: z.string().describe('a rootfs build id from the build tool'),
        app: z.string().optional(),
        port: z.number().int().optional().describe('sets PORT in the service environment'),
        domain: z.string().optional(),
        custom_domain: z.string().optional(),
        health: z
          .object({
            type: z.string().optional(),
            path: z.string().optional(),
            test: z.array(z.string()).optional(),
            grace: z.number().int().optional(),
          })
          .optional(),
        env: z.record(z.string(), z.string()).optional(),
        secret_env: z.record(z.string(), z.string()).optional(),
        replicas: z.number().int().positive().optional(),
      },
    },
    (args) => wrap(() => deployService(client, args)),
  )

  server.registerTool(
    'promote',
    {
      title: 'Promote a machine',
      description:
        'Turn a sandbox into a durable service. The URL does not change, which is the whole point: ' +
        'every link to the sandbox keeps working against the service.',
      inputSchema: {
        machine: machineArg,
        custom_domain: z.string().optional(),
        replicas: z.number().int().positive().optional(),
      },
    },
    (args) =>
      wrap(async () => {
        const machine = await resolveMachine(client, args.machine)
        return await client.machines.promote(machine.id, {
          ...(args.custom_domain ? { custom_domain: args.custom_domain } : {}),
          ...(args.replicas ? { replicas: args.replicas } : {}),
        })
      }),
  )

  server.registerTool(
    'destroy_machine',
    {
      title: 'Destroy a machine',
      description: 'Destroy a machine and its snapshots. Irreversible.',
      inputSchema: { machine: machineArg },
    },
    (args) =>
      wrap(async () => {
        const machine = await resolveMachine(client, args.machine)
        await client.machines.destroy(machine.id)
        return { destroyed: machine.id }
      }),
  )

  server.registerTool(
    'generate_dockerfile',
    {
      title: 'Generate a Dockerfile',
      description:
        'Detect the framework in a directory and return a Dockerfile for it, with the port and health check to deploy it with. ' +
        'Use this before `build` on a repo that has no Dockerfile. ' +
        DOCKERFILE_RULES,
      inputSchema: {
        dir: z.string(),
        write: z.boolean().default(false).describe('also write the Dockerfile, if the directory has none'),
      },
    },
    (args) =>
      wrap(async () => {
        const recipe = generateDockerfile(args.dir)
        if (recipe.framework === 'unknown') {
          throw new UnknownFrameworkError(recipe.notes.join('\n'))
        }
        if (args.write) {
          const { existsSync, writeFileSync } = await import('node:fs')
          const { join } = await import('node:path')
          const path = join(args.dir, 'Dockerfile')
          // Never overwritten: a Dockerfile already in the tree is the repo's
          // own answer, and resolution order puts it first. The check has to
          // be taken BEFORE the write and kept: re-reading it afterwards is
          // true either way, which would report a recipe as written when the
          // repo's own file is what the build will actually use.
          const existed = existsSync(path)
          if (!existed) writeFileSync(path, recipe.dockerfile)
          return { ...recipe, written: !existed, path }
        }
        return recipe
      }),
  )
}

/** Raised when detection found nothing; the notes list every file looked for. */
class UnknownFrameworkError extends Error {}

/**
 * Runs a handler and shapes the result.
 *
 * Every failure becomes `isError: true` with the most actionable text
 * available: the server's own body for an API error, every NDJSON line for a
 * failed build, and the message otherwise.
 */
async function wrap(fn: () => unknown | Promise<unknown>): Promise<ToolResult> {
  try {
    const result = await fn()
    return { content: [{ type: 'text', text: JSON.stringify(result, null, 2) }] }
  } catch (err) {
    return { content: [{ type: 'text', text: errorText(err) }], isError: true }
  }
}

function errorText(err: unknown): string {
  if (err instanceof BuildFailedError) {
    // Every line, verbatim, one JSON object per line. This is what the agent
    // reads to find the failing step.
    return err.lines.map((line) => JSON.stringify(line)).join('\n')
  }
  if (err instanceof PilotsError && err.body) {
    // The server's body unchanged, so a 429 reaches the agent exactly as hostd
    // wrote it and matches what the CLI and the SDK show.
    return err.body
  }
  return err instanceof Error ? err.message : String(err)
}

interface DeployArgs {
  name: string
  build: string
  app?: string | undefined
  port?: number | undefined
  domain?: string | undefined
  custom_domain?: string | undefined
  health?: HealthCheck | undefined
  env?: Record<string, string> | undefined
  secret_env?: Record<string, string> | undefined
  replicas?: number | undefined
}

async function deployService(
  client: PilotsClient,
  args: DeployArgs,
): Promise<{ service_id: string; url: string; release_id: string }> {
  // `port` is not a field on the service row: the platform learns the port the
  // same way every recipe sets it, through PORT in the environment.
  const env = args.port !== undefined ? { ...args.env, PORT: String(args.port) } : args.env

  const services = await client.services.list()
  const existing = services.find((s) => s.name === args.name && (s.app ?? '') === (args.app ?? ''))

  let service: Service
  if (existing) {
    const req: UpdateServiceRequest = {
      ...(args.replicas !== undefined ? { replicas: args.replicas } : {}),
      ...(args.health ? { health: args.health } : {}),
      ...(env ? { env } : {}),
      ...(args.secret_env ? { secret_env: args.secret_env } : {}),
    }
    service = await client.services.patch(existing.id, req)
  } else {
    const req: CreateServiceRequest = {
      name: args.name,
      build: args.build,
      ...(args.app ? { app: args.app } : {}),
      ...(args.replicas !== undefined ? { replicas: args.replicas } : {}),
      ...(args.health ? { health: args.health } : {}),
      ...(args.domain ? { domain: args.domain } : {}),
      ...(args.custom_domain ? { custom_domain: args.custom_domain } : {}),
      ...(env ? { env } : {}),
      ...(args.secret_env ? { secret_env: args.secret_env } : {}),
    }
    service = await client.services.create(req)
  }

  const release = await client.services.deploy(service.id, { build: args.build })
  const deadline = Date.now() + 10 * 60 * 1000
  for (;;) {
    const current = await client.services.get(service.id)
    if (current.release_id === release.id) {
      return {
        service_id: service.id,
        url: current.custom_domain || current.url || '',
        release_id: release.id,
      }
    }
    if (Date.now() >= deadline) {
      throw new Error(`release ${release.id} did not become current within 600s`)
    }
    await new Promise((r) => setTimeout(r, 1000))
  }
}
