/**
 * The compose executor.
 *
 * hostd parses compose and returns an ordered plan (#7 Decision 5); this walks
 * that plan against the ordinary endpoints. Nothing here is compose-aware
 * beyond the plan's field names, which is what keeps the parser in one place
 * -- in Go, beside the daemon -- instead of two that drift.
 */

import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'

import type {
  BuildLogLine,
  ComposePlan,
  ComposeStep,
  CreateServiceRequest,
  PilotsClient,
  Service,
  UpdateServiceRequest,
  Volume,
} from '@pilots/sdk'

import type { Credentials } from '../config.ts'
import { CliError } from '../output.ts'
import { tarDirectory, tarFiles } from '../tar.ts'
import { collectRefs, resolveSecrets } from './secrets.ts'

export interface DeployedService {
  name: string
  id: string
  url: string
  release_id: string
  rootfs_build_id: string
}

export interface DeployResult {
  app: string
  services: DeployedService[]
}

export interface ExecuteOptions {
  /** The compose file's own directory; every build context is relative to it. */
  dir: string
  credentials?: Credentials | null
  env?: NodeJS.ProcessEnv
  /** Poll for the new release before returning. */
  wait?: boolean
  waitTimeoutMs?: number
  onBuildLine?: (step: ComposeStep, line: BuildLogLine) => void
  sleep?: (ms: number) => Promise<void>
  now?: () => number
}

const DEFAULT_WAIT_MS = 10 * 60 * 1000
const PRE_DEPLOY_TIMEOUT_MS = 10 * 60 * 1000

export async function executePlan(
  client: PilotsClient,
  plan: ComposePlan,
  opts: ExecuteOptions,
): Promise<DeployResult> {
  const sleep = opts.sleep ?? ((ms: number) => new Promise<void>((r) => setTimeout(r, ms)))
  const now = opts.now ?? (() => Date.now())
  const app = plan.app

  // Every secret for every step, resolved before the first request. A build
  // that succeeds and then stops because the third service is missing a
  // password has already spent minutes and left half an app deployed.
  // Resolved for the whole plan at once, so every missing name is reported in
  // one message. One name per failed deploy would mean one failed deploy per
  // secret.
  const resolveOpts = {
    app,
    ...(opts.env ? { env: opts.env } : {}),
    ...(opts.credentials !== undefined ? { credentials: opts.credentials } : {}),
  }
  const everyRef: Record<string, string> = {}
  for (const name of collectRefs(plan.steps)) everyRef[name] = name
  resolveSecrets(everyRef, resolveOpts)

  const secretsByStep = new Map<string, Record<string, string>>()
  for (const step of plan.steps) {
    secretsByStep.set(step.name, resolveSecrets(step.secret_refs, resolveOpts))
  }

  const deployed: DeployedService[] = []
  for (const step of plan.steps) {
    const secretEnv = secretsByStep.get(step.name) ?? {}
    // One volume per service, because one machine mounts a volume and a
    // volume-backed service runs one machine. Refused BEFORE the build, so a
    // compose file that asks for two does not spend minutes on an image first.
    if (step.volumes && step.volumes.length > 1) {
      throw new CliError(
        `${step.name}: declares ${step.volumes.length} volumes, and a service mounts one; ` +
          'split the service or merge the mounts',
      )
    }
    // Beside it, and for the same reason: a service's volume is set when the
    // service is created, so a compose file that changes one is refused --
    // before the build, and before ensureVolumes, which CREATES the volume it
    // cannot find by name. Refusing after it costs a build and leaves a new,
    // empty, billed volume behind that nothing will ever mount.
    const existing = await findService(client, app, step.name)
    await refuseVolumeChange(client, app, step, existing)

    const rootfs = await buildStep(client, step, opts)
    const [volume] = await ensureVolumes(client, app, step)
    await runPreDeploy(client, app, step, rootfs, secretEnv)
    const service = await upsertService(client, app, step, rootfs, secretEnv, existing, volume?.id)
    // Knobs travel on the deploy, not on the create: a service row has
    // nowhere to keep them, and the create and the first deploy are separate
    // requests. Sending them here is also what makes a redeploy with changed
    // knobs actually change them.
    const release = await client.services.deploy(service.id, {
      build: rootfs,
      ...(step.knobs ? { knobs: step.knobs } : {}),
    })

    let releaseId = release.id
    if (opts.wait !== false) {
      releaseId = await waitForRelease(client, service.id, release.id, {
        sleep,
        now,
        timeoutMs: opts.waitTimeoutMs ?? DEFAULT_WAIT_MS,
      })
    }
    const current = await client.services.get(service.id)
    deployed.push({
      name: step.name,
      id: service.id,
      url: current.custom_domain || current.url || '',
      release_id: releaseId,
      rootfs_build_id: rootfs,
    })
  }

  return { app, services: deployed }
}

/**
 * Builds one step, whatever its shape.
 *
 * A stock-image step is a build too. The plan hands back `dockerfile: "FROM
 * postgres:17\n"` and no build context, so a one-file tar goes up the same
 * route: hostd turns any Dockerfile into a bootable rootfs, and a second code
 * path for "just pull this image" would be a second thing to keep correct.
 */
async function buildStep(client: PilotsClient, step: ComposeStep, opts: ExecuteOptions): Promise<string> {
  let tar: Buffer
  if (step.build) {
    const context = resolve(opts.dir, step.build.context ?? '.')
    const dockerfile = step.build.dockerfile
    const extras =
      dockerfile && dockerfile !== 'Dockerfile'
        ? { Dockerfile: readDockerfile(resolve(context, dockerfile)) }
        : undefined
    tar = tarDirectory(context, extras ? { extraFiles: extras } : {})
  } else if (step.dockerfile) {
    tar = tarFiles({ Dockerfile: step.dockerfile })
  } else {
    throw new CliError(`${step.name}: the plan carries neither a build context nor a Dockerfile`)
  }

  const stream = await client.builds.create(new Uint8Array(tar))
  for await (const line of stream) opts.onBuildLine?.(step, line)
  return await stream.result()
}

function readDockerfile(path: string): string {
  // Read here rather than left in the context, because hostd builds the
  // `Dockerfile` at the tar's root. A compose file naming `Dockerfile.prod`
  // would otherwise build the wrong file with no error.
  try {
    return readFileSync(path, 'utf8')
  } catch (err) {
    throw new CliError(`cannot read ${path}: ${(err as Error).message}`)
  }
}

/** Creates the volumes a step declares, skipping any that already exist. */
async function ensureVolumes(client: PilotsClient, app: string, step: ComposeStep): Promise<Volume[]> {
  if (!step.volumes || step.volumes.length === 0) return []
  const existing = await client.volumes.list()
  const out: Volume[] = []
  for (const want of step.volumes) {
    // Prefixed with the app so two apps in one org can each have a `pgdata`.
    const name = `${app}-${want.name}`
    const found = existing.find((v) => v.name === name)
    out.push(
      found ??
        (await client.volumes.create({
          name,
          size_gib: want.size_gib,
          mount_path: want.mount_path,
        })),
    )
  }
  return out
}

/**
 * Runs `x-pilots.pre_deploy` as a one-shot machine, then destroys it.
 *
 * uncloud's pre-deploy hook shape (`pkg/client/compose/service.go:497-501`): a
 * job's lifecycle is never a service's. Running a migration as a service
 * replica means the autoscaler owns it, which is how a migration ends up
 * running once per replica.
 */
async function runPreDeploy(
  client: PilotsClient,
  app: string,
  step: ComposeStep,
  rootfs: string,
  secretEnv: Record<string, string>,
): Promise<void> {
  if (!step.pre_deploy) return
  const machine = await client.machines.create({
    name: `${app}-${step.name}-predeploy-${Math.floor(Date.now() / 1000)}`,
    image: rootfs,
    app,
    ...(step.env ? { env: step.env } : {}),
    ...(Object.keys(secretEnv).length > 0 ? { secret_env: secretEnv } : {}),
    knobs: { auto_stop: 'off', auto_start: false },
  })
  try {
    const result = await client.machines.exec(machine.id, {
      cmd: step.pre_deploy,
      cwd: '/app',
      ...(step.env ? { env: step.env } : {}),
      timeout_ms: PRE_DEPLOY_TIMEOUT_MS,
    })
    if (result.exit_code !== 0) {
      if (result.stdout) process.stderr.write(result.stdout)
      if (result.stderr) process.stderr.write(result.stderr)
      throw new CliError(`${step.name}: pre_deploy exited ${result.exit_code}`)
    }
  } finally {
    // Destroyed whether it passed or failed. A failed migration that left a
    // machine behind bills for it forever.
    await client.machines.destroy(machine.id).catch(() => {})
  }
}

/** The service a step deploys, if this app already has one by that name. */
async function findService(
  client: PilotsClient,
  app: string,
  name: string,
): Promise<Service | undefined> {
  const services = await client.services.list()
  return services.find((s) => s.name === name && (s.app ?? '') === app)
}

/**
 * Refuses a deploy that would change which volume a service mounts.
 *
 * Create-only, like knobs: hostd's PATCH carries no volume field, and nothing
 * anywhere copies data between two volumes, so this is a data migration
 * wearing a one-line compose edit. The name is what decides it, and the name
 * is known before anything has been built or created.
 */
async function refuseVolumeChange(
  client: PilotsClient,
  app: string,
  step: ComposeStep,
  existing: Service | undefined,
): Promise<void> {
  if (!existing) return
  const want = step.volumes?.[0]
  const wantName = want ? `${app}-${want.name}` : ''
  const mountedId = existing.volume_id ?? ''
  if (!mountedId && !wantName) return

  const volumes = await client.volumes.list()
  if (mountedId && wantName && volumes.find((v) => v.name === wantName)?.id === mountedId) return

  const mountedName = volumes.find((v) => v.id === mountedId)?.name || mountedId
  throw new CliError(
    `${step.name}: mounts ${mountedName || '(no volume)'} and the compose file names ` +
      `${wantName || '(no volume)'}; a service's volume is set when it is created`,
  )
}

async function upsertService(
  client: PilotsClient,
  app: string,
  step: ComposeStep,
  rootfs: string,
  secretEnv: Record<string, string>,
  existing: Service | undefined,
  volumeId?: string,
): Promise<Service> {
  if (!existing) {
    const req: CreateServiceRequest = {
      name: step.name,
      app,
      build: rootfs,
      replicas: step.replicas,
      ...(step.health ? { health: step.health } : {}),
      ...(step.domain ? { domain: step.domain } : {}),
      ...(step.custom_domain ? { custom_domain: step.custom_domain } : {}),
      ...(volumeId ? { volume: volumeId } : {}),
      ...(step.env ? { env: step.env } : {}),
      ...(Object.keys(secretEnv).length > 0 ? { secret_env: secretEnv } : {}),
    }
    return await client.services.create(req)
  }

  const req: UpdateServiceRequest = {
    replicas: step.replicas,
    ...(step.health ? { health: step.health } : {}),
    ...(step.env ? { env: step.env } : {}),
    ...(Object.keys(secretEnv).length > 0 ? { secret_env: secretEnv } : {}),
  }
  return await client.services.patch(existing.id, req)
}

/** Polls until the service reports the release just created, or gives up. */
async function waitForRelease(
  client: PilotsClient,
  serviceId: string,
  releaseId: string,
  opts: { sleep: (ms: number) => Promise<void>; now: () => number; timeoutMs: number },
): Promise<string> {
  const deadline = opts.now() + opts.timeoutMs
  for (;;) {
    const service = await client.services.get(serviceId)
    if (service.release_id === releaseId) return releaseId
    if (opts.now() >= deadline) {
      throw new CliError(
        `release ${releaseId} did not become current within ${Math.round(opts.timeoutMs / 1000)}s`,
      )
    }
    await opts.sleep(1000)
  }
}
