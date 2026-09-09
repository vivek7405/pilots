/**
 * The tools.
 *
 * Three rules run through all of them. Every result carries `next`: the call
 * to make now, or the empty string when the loop is done. It is on the result
 * rather than only in the description because a small model that has stopped
 * reading descriptions is still reading results, and the one thing it needs at
 * that moment is what to do next.
 *
 * The other two. First, a result is JSON text, so an agent
 * parses it rather than reads it. Second, an error carries the SERVER'S body
 * verbatim: a 429 reaches the agent exactly as hostd wrote it, and a failed
 * build carries every NDJSON line, because reading the failing step and
 * patching the Dockerfile is the loop the structured log exists for.
 *
 * A non-zero exit from `exec` is NOT a tool error. The command ran; what its
 * status means is the agent's call, and marking it an error would make every
 * `grep` that found nothing look like a broken tool.
 */

import { basename } from 'node:path'

import type { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js'
import { z } from 'zod'

import {
  BuildFailedError,
  PilotsError,
  type BuildLogLine,
  type ComposePlan,
  type CreateServiceRequest,
  type HealthCheck,
  type PilotsClient,
  type Service,
  type UpdateServiceRequest,
} from '@pilots/sdk'

import { loadCredentials } from '../config.ts'
import { executePlan } from '../compose/run.ts'
import { resolveMachine } from '../resolve.ts'
import { tarDirectory } from '../tar.ts'
import { PRIMER } from './primer.ts'
import { readTopic, searchTopics, topics } from './skill.ts'

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
    (args) => wrap(() => client.machines.create(args), 'exec on the returned id'),
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
      }, ''),
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
      }, ''),
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
      }, ''),
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
      }, ''),
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
      }, ''),
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
      }, (r: { id: string }) => `restore with checkpoint=${r.id}`),
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
    (args) => wrap(() => client.checkpoints.restore(args.checkpoint), 'status on the machine'),
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
      }, (r: { rootfs_build_id: string }) => `deploy with name and build=${r.rootfs_build_id}`),
  )

  server.registerTool(
    'deploy',
    {
      title: 'Deploy',
      description:
        'Deploy a directory to a URL in one call: plans it on the host, builds each service, deploys, ' +
        'waits for the health gate, and returns { app, services: [{ name, url }], next }. ' +
        'Call this FIRST for any "deploy this" request, with `dir` and nothing else unless the user gave more. ' +
        'No directory and no repository in the conversation: ask, never invent a path. ' +
        'Also accepts `name` + `build` for a rootfs you built yourself. ' +
        'On unknown_framework read `details` and call `build` with a Dockerfile you write; ' +
        'on health_gate_failed call `diagnose` with `details.replica`.',
      inputSchema: {
        dir: z.string().optional().describe('the directory to deploy; the host decides what it is'),
        name: z.string().optional().describe('the service name, for the name + build form'),
        build: z.string().optional().describe('a rootfs build id, for the name + build form'),
        app: z.string().optional(),
        port: z.number().int().optional().describe('sets PORT in the service environment'),
        domain: z.string().optional(),
        private: z
          .boolean()
          .optional()
          .describe('mint no address; peers still reach it at <name>.internal'),
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
    (args) =>
      wrap(async () => {
        // The directory form is the front door and runs exactly what
        // `pilot deploy` runs: the same plan route, the same executor. A
        // second implementation here would drift from the CLI on the day it
        // mattered, and the drift would only show up on a real deploy.
        if (args.dir) {
          const plan = (await client.plan(new Uint8Array(tarDirectory(args.dir)), { app: basename(args.dir) })).plan
          if (args.app) plan.app = args.app
          applyOverrides(plan, args)
          // A failed build throws a BuildFailedError carrying every NDJSON
          // line, and errorText below returns them verbatim, so nothing here
          // has to collect them a second time.
          const result = await executePlan(client, plan, {
            dir: args.dir,
            credentials: loadCredentials(),
            wait: true,
          })
          return { app: result.app, services: result.services }
        }
        if (!args.name || !args.build) {
          throw new Error(
            'pass dir to deploy a directory, or name and build to deploy a rootfs you already built',
          )
        }
        return await deployService(client, { ...args, name: args.name, build: args.build })
      }, 'report the URL; the deploy is done'),
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
      }, (r: { id: string }) => `service with service=${r.id}`),
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
        // The host decides, from the same tar a build would upload. An
        // `unknown_framework` refusal passes through as the tool error, body
        // and all, so the agent gets the listing and the rules rather than a
        // sentence saying it failed.
        const res = await client.plan(new Uint8Array(tarDirectory(args.dir)))
        const recipes = res.detected
          .map((d, i) => ({ detected: d, step: res.plan.steps[i] }))
          .filter(({ detected }) => detected.source === 'recipe')
          .map(({ detected, step }) => ({
            service: detected.service,
            framework: detected.framework,
            dir: detected.dir,
            dockerfile: step?.dockerfile ?? '',
            port: detected.port,
            health: detected.health,
            notes: detected.notes ?? [],
          }))
        if (recipes.length === 0) {
          throw new Error(
            'this directory already has a compose file or a Dockerfile, so the platform ' +
              'would build that rather than generate one; deploy it with `deploy`',
          )
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
          if (!existed && recipes.length === 1) writeFileSync(path, recipes[0]!.dockerfile)
          return { recipes, written: !existed && recipes.length === 1, path }
        }
        return { recipes }
      }, 'deploy with the same dir'),
  )

  server.registerTool(
    'plan',
    {
      title: 'Plan a directory',
      description:
        'Show what `deploy` would do without building anything: the services, the source of each ' +
        '(compose, dockerfile, recipe), the ports, the health checks, and any generated Dockerfile. ' +
        'Call it when asked what will happen, or to check how a monorepo splits before a build. ' +
        'Next: deploy with the same dir.',
      inputSchema: {
        dir: z.string().describe('the directory to plan'),
        app: z.string().optional(),
      },
    },
    (args) =>
      wrap(
        () => client.plan(new Uint8Array(tarDirectory(args.dir)), { app: args.app ?? basename(args.dir) }),
        `deploy with dir=${args.dir}`,
      ),
  )

  server.registerTool(
    'build_logs',
    {
      title: 'Replay a build log',
      description:
        "Replay a build's log by build_id, the id a failed deploy or build named. " +
        'Call it when a result says a build failed and you did not see the lines. Read-only.',
      inputSchema: { build_id: z.string() },
    },
    (args) =>
      wrap(async () => {
        const stream = await client.builds.logs(args.build_id)
        const lines: BuildLogLine[] = []
        for await (const line of stream) lines.push(line)
        return { build_id: args.build_id, lines }
      }, 'fix what the line carrying error names, then build again'),
  )

  server.registerTool(
    'list_services',
    {
      title: 'List services',
      description:
        'Every service this key can see, with url, release_id and replicas. ' +
        'Call it to find a name before service, releases or logs. Read-only.',
      inputSchema: {},
    },
    () => wrap(() => client.services.list(), 'service with one of these names'),
  )

  server.registerTool(
    'service',
    {
      title: 'One service',
      description:
        'One service by name or id: its health check, its env KEYS (never the values), its domain, ' +
        'its current release and its replica ids. Read-only. ' +
        'Next: releases for history, logs on a replica.',
      inputSchema: { service: z.string().describe('a service id or name') },
    },
    (args) =>
      wrap(async () => {
        const svc = await resolveService(client, args.service)
        const machines = await client.machines.list()
        return {
          ...svc,
          replica_ids: machines.filter((m) => m.app === svc.app && m.name.startsWith(svc.name)).map((m) => m.id),
        }
      }, 'releases for history, or logs on a replica id'),
  )

  server.registerTool(
    'releases',
    {
      title: 'A service\'s releases',
      description:
        "A service's releases, newest first, with healthy and the build each came from. " +
        'Call it before rollback. Read-only.',
      inputSchema: { service: z.string() },
    },
    (args) =>
      wrap(async () => {
        const svc = await resolveService(client, args.service)
        return await client.services.releases(svc.id)
      }, 'rollback only if the user agrees to change what is serving'),
  )

  server.registerTool(
    'rollback',
    {
      title: 'Roll a service back',
      description:
        'Roll a service back to its previous healthy release. ' +
        'This CHANGES WHAT IS SERVING: say so and get agreement before calling it. ' +
        'Next: service, to confirm release_id moved.',
      inputSchema: { service: z.string() },
    },
    (args) =>
      wrap(async () => {
        const svc = await resolveService(client, args.service)
        return await client.services.rollback(svc.id)
      }, 'service, to confirm release_id moved'),
  )

  server.registerTool(
    'domains',
    {
      title: 'List custom domains',
      description:
        'Every custom domain on this key\'s services, with whether each is verified. Read-only: ' +
        'adding one is `pilot domains add`, because it needs a DNS record the user creates.',
      inputSchema: {},
    },
    () => wrap(() => client.domains.list(), ''),
  )

  server.registerTool(
    'volumes',
    {
      title: 'List volumes',
      description:
        'Every volume, with the machine each is attached to. Read-only: a volume is declared in a ' +
        'compose file, not created by hand.',
      inputSchema: {},
    },
    () => wrap(() => client.volumes.list(), ''),
  )

  server.registerTool(
    'diagnose',
    {
      title: 'Diagnose a failed deploy',
      description:
        "Explain a failed deploy: the replica's last console lines and what it is doing. " +
        'Call it right after health_gate_failed with details.replica. ' +
        'Deterministic, no model. Next: fix the app and deploy again, or rollback.',
      inputSchema: {
        replica: machineArg.describe('the replica id from a health_gate_failed error'),
        tail: z.number().int().positive().default(80),
      },
    },
    (args) =>
      wrap(async () => {
        const machine = await resolveMachine(client, args.replica)
        const text = await client.machines.logs(machine.id)
        const lines = text.split('\n')
        return {
          replica: machine.id,
          state: machine.state,
          url: machine.url,
          // The console is what a health gate failure is actually about: the
          // app either did not start, or started on the wrong address. Both
          // say so here and nowhere else the platform can see.
          tail: lines.slice(Math.max(0, lines.length - (args.tail ?? 80))).join('\n'),
          checks: [
            'is the app listening on 0.0.0.0 rather than 127.0.0.1?',
            'does it read $PORT, with 8080 as the fallback?',
            'did it exit before it bound anything? the last lines say so',
          ],
        }
      }, 'fix the app and deploy again, or rollback'),
  )

  server.registerTool(
    'init',
    {
      title: 'Read this first',
      description:
        'READ THIS FIRST. The pilots mental model in under sixty lines: one primitive, the one-call ' +
        'deploy, what every result and error carries, and the doc index. ' +
        'Call it once at the start of any pilots task. Read-only.',
      inputSchema: {},
    },
    () => wrap(() => ({ primer: PRIMER, topics: topics() }), 'deploy with dir, once you know the directory'),
  )

  server.registerTool(
    'docs',
    {
      title: 'Read a reference',
      description:
        'Read one pilots reference by topic (deploy, sandboxes, services, secrets, volumes, domains, ' +
        'promote, errors, compose), or search them with query. No arguments lists the topics. ' +
        'Load one. Two at most. Read-only.',
      inputSchema: {
        topic: z.string().optional(),
        query: z.string().optional().describe('search every reference instead of naming one'),
      },
    },
    (args) =>
      wrap(() => {
        if (args.query) return { query: args.query, matches: searchTopics(args.query) }
        if (!args.topic) return { topics: topics() }
        const text = readTopic(args.topic)
        if (text === null) {
          throw new Error(`no such topic ${args.topic}; the topics are ${topics().join(', ')}`)
        }
        return { topic: args.topic, text }
      }, ''),
  )
}

/**
 * Applies a caller's per-service overrides onto a planned step.
 *
 * A `health` or a `replicas` passed alongside `dir` and then quietly dropped
 * is the worst outcome available: the deploy succeeds, the gate polls
 * something else, and nothing anywhere says the argument was ignored. So it is
 * applied, and where it CANNOT be applied unambiguously -- a monorepo, where
 * "the service" is two of them -- it is refused with a message naming the fix.
 */
function applyOverrides(plan: ComposePlan, args: DeployOverrides): void {
  const overrides = ['port', 'health', 'env', 'secret_env', 'replicas', 'domain', 'custom_domain'] as const
  const given = overrides.filter((key) => args[key] !== undefined)
  if (given.length === 0) return
  if (plan.steps.length !== 1) {
    throw new Error(
      `this directory plans ${plan.steps.length} services, so ${given.join(', ')} ` +
        'cannot be applied to one of them; put them in a compose file',
    )
  }
  const step = plan.steps[0]!
  if (args.port !== undefined) step.env = { ...step.env, PORT: String(args.port) }
  if (args.env) step.env = { ...step.env, ...args.env }
  if (args.health) step.health = args.health
  if (args.replicas !== undefined) step.replicas = args.replicas
  if (args.domain) step.domain = args.domain
  if (args.private) step.private = true
  if (args.custom_domain) step.custom_domain = args.custom_domain
  if (args.secret_env) {
    // A value, not a reference: the compose path resolves `secret://` names
    // from the local store, and this one already has the values in hand.
    throw new Error(
      'secret_env with dir is not supported: put secret:// references in a compose file, ' +
        'or deploy with name and build',
    )
  }
}

interface DeployOverrides {
  port?: number | undefined
  health?: HealthCheck | undefined
  env?: Record<string, string> | undefined
  secret_env?: Record<string, string> | undefined
  replicas?: number | undefined
  domain?: string | undefined
  private?: boolean | undefined
  custom_domain?: string | undefined
}

/** A service by id or name, so every tool takes whichever the agent has. */
async function resolveService(client: PilotsClient, ref: string): Promise<Service> {
  const services = await client.services.list()
  const found = services.find((s) => s.id === ref || s.name === ref)
  if (!found) {
    throw new Error(`no service ${ref}; list_services shows what this key can see`)
  }
  return found
}

/**
 * Runs a handler and shapes the result.
 *
 * `next` is folded into the result object here rather than by each handler, so
 * a tool cannot ship without one. It is a function of the result where the
 * next step depends on what came back, and a constant otherwise.
 *
 * Every failure becomes `isError: true` with the most actionable text
 * available: the server's own body for an API error, every NDJSON line for a
 * failed build, and the message otherwise. The server's body already carries
 * its own `code`, `next` and `details`, so nothing is added on that path.
 */
async function wrap(
  fn: () => unknown | Promise<unknown>,
  next: string | ((result: never) => string) = '',
): Promise<ToolResult> {
  try {
    const result = await fn()
    const step = typeof next === 'function' ? next(result as never) : next
    const body =
      result !== null && typeof result === 'object' && !Array.isArray(result)
        ? { ...(result as object), next: step }
        : { result, next: step }
    return { content: [{ type: 'text', text: JSON.stringify(body, null, 2) }] }
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
  private?: boolean | undefined
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
      ...(args.private ? { private: true } : {}),
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
