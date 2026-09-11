/**
 * A hostd stand-in.
 *
 * Answers the routes the CLI drives with the shapes `internal/api/types.go`
 * declares, records every request, and lets a test script one route at a time.
 * No test in this package reaches a real fleet, so what a command SENDS is the
 * thing under test, and this records exactly that.
 */

import type { ServerResponse } from 'node:http'

import { json, startServer, type FakeServer, type RecordedRequest } from './server.ts'

export interface FakeAPI extends FakeServer {
  /** Route key `"POST /v1/machines"` to a handler; consulted before the defaults. */
  routes: Map<string, (req: { path: string; body: string }, res: ServerResponse) => void>
  machines: Machine[]
  services: Service[]
  volumes: Volume[]
  find: (method: string, pathPrefix: string) => RecordedRequest | undefined
  all: (method: string, pathPrefix: string) => RecordedRequest[]
}

interface Machine {
  id: string
  name: string
  host_id: string
  /** Set on a replica; what `pilot logs <service>` fans in by. */
  service_id?: string
  state: string
  knobs: { auto_stop: string; auto_start: boolean; min_machines_running: number; soft_limit: number }
  vcpus: number
  mem_mib: number
  url: string
  app?: string
  created_at: number
  last_activity: number
}

interface Service {
  id: string
  name: string
  app?: string
  replicas: number
  knobs: Machine['knobs']
  url?: string
  release_id?: string
  autodeploy: boolean
  created_at: number
}

interface Volume {
  id: string
  name: string
  size_gib: number
  mount_path: string
  created_at: number
}

const KNOBS = { auto_stop: 'suspend', auto_start: true, min_machines_running: 0, soft_limit: 20 }

export function fakeMachine(over: Partial<Machine> = {}): Machine {
  const id = over.id ?? `m_${Math.random().toString(36).slice(2, 10)}`
  const name = over.name ?? id
  return {
    id,
    name,
    host_id: 'host-a',
    state: 'running',
    knobs: KNOBS,
    vcpus: 1,
    mem_mib: 512,
    url: `https://${name}.pilotrun.app`,
    created_at: 1,
    last_activity: 1,
    ...over,
  }
}

export function fakeService(over: Partial<Service> = {}): Service {
  const id = over.id ?? `svc_${Math.random().toString(36).slice(2, 10)}`
  return {
    id,
    name: over.name ?? id,
    replicas: 1,
    knobs: KNOBS,
    url: `https://${over.name ?? id}.pilotrun.app`,
    autodeploy: false,
    created_at: 1,
    ...over,
  }
}

export async function startFakeAPI(): Promise<FakeAPI> {
  const state: Pick<FakeAPI, 'machines' | 'services' | 'volumes'> = {
    machines: [],
    services: [],
    volumes: [],
  }
  const routes = new Map<string, (req: { path: string; body: string }, res: ServerResponse) => void>()
  let buildCount = 0

  const server = await startServer((req, res) => {
    const method = req.method ?? 'GET'
    const path = req.path
    const scripted = routes.get(`${method} ${path}`)
    if (scripted) return scripted(req, res)

    // The plan route. The real one runs the detector in Go, which is tested
    // there; what a test in this package cares about is that the CLI tarred
    // the directory and executed the answer, so the default answer is one
    // recipe step and a test that wants another scripts the route.
    if (method === 'POST' && path === '/v1/plan') {
      return json(res, 200, defaultPlanResponse())
    }

    // Builds. The status is 200 before the outcome is known, so the verdict
    // is the LAST NDJSON line rather than the code.
    if (method === 'POST' && path === '/v1/builds') {
      buildCount++
      const id = `bld_${buildCount}`
      res.writeHead(200, { 'content-type': 'application/x-ndjson', 'x-pilot-build-id': id })
      res.write(JSON.stringify({ step: 'FROM', line: 'pulling base', ts: 1 }) + '\n')
      res.write(JSON.stringify({ result: `rootfs_${buildCount}`, ts: 2 }) + '\n')
      return res.end()
    }

    // Machines.
    if (method === 'POST' && path === '/v1/machines') {
      const body = JSON.parse(req.body || '{}') as Partial<Machine>
      const machine = fakeMachine({ ...(body.name ? { name: body.name } : {}), ...(body.app ? { app: body.app } : {}) })
      state.machines.push(machine)
      return json(res, 201, machine)
    }
    if (method === 'GET' && path === '/v1/machines') return json(res, 200, state.machines)
    const machineId = matchOne(path, '/v1/machines/')
    if (machineId) {
      const machine = state.machines.find((m) => m.id === machineId)
      if (method === 'GET' && path === `/v1/machines/${machineId}`) {
        return machine ? json(res, 200, machine) : json(res, 404, { error: 'no such machine' })
      }
      if (method === 'DELETE') {
        state.machines = state.machines.filter((m) => m.id !== machineId)
        res.writeHead(204)
        return res.end()
      }
      if (method === 'POST' && path.endsWith('/exec')) {
        return json(res, 200, { stdout: 'ok\n', stderr: '', exit_code: 0 })
      }
      if (method === 'GET' && path.endsWith('/logs')) {
        res.writeHead(200, { 'content-type': 'text/plain' })
        return res.end('line one\nline two\n')
      }
      if (method === 'POST' && path.endsWith('/checkpoints')) {
        return json(res, 201, { id: 'cp_1', machine_id: machineId, seq: 1, durable: false, created_at: 1 })
      }
      if (method === 'POST' && path.endsWith('/promote')) {
        const svc = fakeService({ name: machine?.name ?? 'promoted' })
        state.services.push(svc)
        return json(res, 200, svc)
      }
      if (method === 'POST') {
        res.writeHead(204)
        return res.end()
      }
    }

    // Services.
    if (method === 'POST' && path === '/v1/services') {
      const body = JSON.parse(req.body || '{}') as Partial<Service> & { volume?: string }
      const svc = fakeService({
        ...(body.name ? { name: body.name } : {}),
        ...(body.app ? { app: body.app } : {}),
        ...(body.replicas !== undefined ? { replicas: body.replicas } : {}),
        // Echoed as hostd echoes it, so a second run can see what the service
        // already mounts and refuse a swap.
        ...(body.volume ? { volume_id: body.volume } : {}),
      })
      state.services.push(svc)
      return json(res, 201, svc)
    }
    if (method === 'GET' && path === '/v1/services') return json(res, 200, state.services)
    const serviceId = matchOne(path, '/v1/services/')
    if (serviceId) {
      const svc = state.services.find((s) => s.id === serviceId)
      if (method === 'GET' && path === `/v1/services/${serviceId}`) {
        return svc ? json(res, 200, svc) : json(res, 404, { error: 'no such service' })
      }
      if (method === 'PATCH') {
        if (!svc) return json(res, 404, { error: 'no such service' })
        const body = JSON.parse(req.body || '{}') as Partial<Service>
        if (body.replicas !== undefined) svc.replicas = body.replicas
        return json(res, 200, svc)
      }
      if (method === 'POST' && path.endsWith('/deploy')) {
        const release = { id: `rel_${state.services.length}_${Date.now()}`, service_id: serviceId, healthy: true, created_at: 1 }
        if (svc) svc.release_id = release.id
        return json(res, 201, release)
      }
      if (method === 'POST' && path.endsWith('/rollback')) {
        return json(res, 200, { id: 'rel_prev', service_id: serviceId, healthy: true, created_at: 1 })
      }
      if (method === 'GET' && path.endsWith('/releases')) return json(res, 200, [])
    }

    // Volumes, domains, hosts.
    if (method === 'POST' && path === '/v1/volumes') {
      const body = JSON.parse(req.body || '{}') as Partial<Volume>
      const volume: Volume = {
        id: `vol_${state.volumes.length + 1}`,
        name: body.name ?? 'v',
        size_gib: body.size_gib ?? 1,
        mount_path: body.mount_path ?? '/data',
        created_at: 1,
      }
      state.volumes.push(volume)
      return json(res, 201, volume)
    }
    if (method === 'GET' && path === '/v1/volumes') return json(res, 200, state.volumes)
    if (method === 'POST' && path === '/v1/domains') {
      const body = JSON.parse(req.body || '{}') as { hostname: string; service_id: string }
      return json(res, 202, {
        hostname: body.hostname,
        service_id: body.service_id,
        verified: false,
        cname_target: 'fleet.pilotrun.app',
        created_at: 1,
      })
    }
    if (method === 'GET' && path === '/v1/domains') return json(res, 200, [])
    if (method === 'DELETE' && path.startsWith('/v1/domains/')) {
      res.writeHead(204)
      return res.end()
    }
    if (method === 'GET' && path === '/v1/hosts') {
      return json(res, 200, [
        { id: 'host-a', cpu_free: 8, mem_free_mib: 16384, last_seen: 1, alive: true },
      ])
    }
    if (method === 'GET' && path === '/v1/whoami') {
      return json(res, 200, { org_id: 'org_1', scopes: ['machines', 'deploy', 'admin'], host_id: 'host-a' })
    }
    if (method === 'GET' && path === '/v1/health') {
      return json(res, 200, { ok: true, host_id: 'host-a', reflink: true })
    }

    return json(res, 404, { error: `fake-api has no route for ${method} ${path}` })
  })

  const api: FakeAPI = Object.assign(server, {
    routes,
    get machines() {
      return state.machines
    },
    get services() {
      return state.services
    },
    get volumes() {
      return state.volumes
    },
    find: (method: string, prefix: string) =>
      server.requests.find((r) => r.method === method && r.path.startsWith(prefix)),
    all: (method: string, prefix: string) =>
      server.requests.filter((r) => r.method === method && r.path.startsWith(prefix)),
  }) as FakeAPI
  return api
}

/** The single path segment after `prefix`, or null when the path goes deeper. */
function matchOne(path: string, prefix: string): string | null {
  if (!path.startsWith(prefix)) return null
  const rest = path.slice(prefix.length)
  const id = rest.split('/')[0]
  return id && id.length > 0 ? id : null
}

/**
 * One recipe step, the shape `POST /v1/plan` answers for a directory the host
 * recognised: a build context plus the Dockerfile text the platform generated,
 * which is the pair the executor has to notice.
 */
export function defaultPlanResponse(): unknown {
  return {
    plan: {
      app: 'fx',
      steps: [
        {
          name: 'web',
          build: { context: '.' },
          dockerfile: 'FROM node:24-alpine\nENV PORT=8080\nEXPOSE 8080\nCMD ["npm", "start"]\n',
          env: { PORT: '8080' },
          ports: [8080],
          health: { type: 'http', path: '/__webjs/ready', grace: 40 },
          replicas: 1,
          vcpus: 1,
          mem_mib: 512,
        },
      ],
    },
    detected: [
      {
        service: 'web',
        source: 'recipe',
        framework: 'webjs',
        dir: '.',
        port: 8080,
        health: { type: 'http', path: '/__webjs/ready', grace: 40 },
        notes: ['webjs is buildless.'],
      },
    ],
  }
}

/** The 400 the host answers for a directory it did not recognise. */
export function unknownFrameworkBody(): unknown {
  return {
    error: 'no framework was detected and there is no Dockerfile or compose file',
    code: 'unknown_framework',
    next: 'add a Dockerfile, or run pilot mcp and ask your agent to write one from details',
    details: {
      dir: '.',
      looked_for: [
        'package.json (with a @webjsdev/* dependency)',
        'next.config.{js,ts,mjs,cjs}',
        'react-router.config.* or remix.config.*',
        'package.json with a bare `remix` dependency (Remix 3)',
        'vite.config.*',
        'manage.py with requirements.txt or pyproject.toml',
        'main.py or app.py importing fastapi',
        'Gemfile with bin/rails',
        'go.mod',
        'Cargo.toml',
        'composer.json with artisan',
      ],
      listing: ['README.md'],
      rules: [
        'bind 0.0.0.0, never 127.0.0.1: a service bound to loopback serves only the guest and the router reaches nothing',
        'read the port from $PORT with 8080 as the fallback; the router dials 8080',
      ],
    },
  }
}
