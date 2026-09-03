/**
 * `PilotsClient`: one method per route, grouped by the noun the route acts on.
 *
 * Every host serves this identical API, so the base URL is any host in the
 * fleet (or the fleet's API name). Nothing here is aware of which host owns a
 * machine: hostd forwards a write that arrived at the wrong host itself.
 */

import { Http } from './http.ts'
import type { HttpOptions } from './http.ts'
import type {
  AddDomainRequest,
  APIKeyResponse,
  Checkpoint,
  CheckpointRequest,
  ComposePlan,
  ComposeRequest,
  CreateAPIKeyRequest,
  CreateMachineRequest,
  CreateServiceRequest,
  CreateVolumeRequest,
  DeployRequest,
  DomainResponse,
  ExecRequest,
  ExecResponse,
  HealthResponse,
  Host,
  Machine,
  MachineVolume,
  PromoteRequest,
  QuotaResponse,
  Release,
  RevokeResponse,
  Service,
  UpdateServiceRequest,
  UsageResponse,
  Volume,
} from './types.ts'

export interface ClientOptions extends HttpOptions {}

export class PilotsClient {
  readonly http: Http
  readonly machines: Machines
  readonly checkpoints: Checkpoints
  readonly services: Services
  readonly domains: Domains
  readonly volumes: Volumes
  readonly hosts: Hosts
  readonly apiKeys: APIKeys
  readonly quotas: Quotas
  readonly usage: Usage
  readonly compose: Compose

  constructor(apiKey: string, opts: ClientOptions = {}) {
    this.http = new Http(apiKey, opts)
    this.machines = new Machines(this.http)
    this.checkpoints = new Checkpoints(this.http)
    this.services = new Services(this.http)
    this.domains = new Domains(this.http)
    this.volumes = new Volumes(this.http)
    this.hosts = new Hosts(this.http)
    this.apiKeys = new APIKeys(this.http)
    this.quotas = new Quotas(this.http)
    this.usage = new Usage(this.http)
    this.compose = new Compose(this.http)
  }

  get baseURL(): string {
    return this.http.baseURL
  }

  get apiKey(): string {
    return this.http.apiKey
  }

  /** Liveness. The one route that needs no key. */
  health(): Promise<HealthResponse> {
    return this.http.json<HealthResponse>('GET', '/v1/health')
  }
}

export class Machines {
  private readonly http: Http

  constructor(http: Http) {
    this.http = http
  }

  create(req: CreateMachineRequest = {}): Promise<Machine> {
    return this.http.json<Machine>('POST', '/v1/machines', { body: req })
  }

  list(): Promise<Machine[]> {
    return this.http.json<Machine[]>('GET', '/v1/machines')
  }

  get(id: string): Promise<Machine> {
    return this.http.json<Machine>('GET', `/v1/machines/${encodeURIComponent(id)}`)
  }

  destroy(id: string): Promise<void> {
    return this.http.none('DELETE', `/v1/machines/${encodeURIComponent(id)}`)
  }

  /** Buffered exec. For output nobody wants to hold in memory, use execStream. */
  exec(id: string, req: ExecRequest): Promise<ExecResponse> {
    return this.http.json<ExecResponse>('POST', `/v1/machines/${encodeURIComponent(id)}/exec`, { body: req })
  }

  logs(id: string): Promise<string> {
    return this.http.text('GET', `/v1/machines/${encodeURIComponent(id)}/logs`)
  }

  suspend(id: string): Promise<void> {
    return this.http.none('POST', `/v1/machines/${encodeURIComponent(id)}/suspend`)
  }

  wake(id: string): Promise<void> {
    return this.http.none('POST', `/v1/machines/${encodeURIComponent(id)}/wake`)
  }

  stop(id: string): Promise<void> {
    return this.http.none('POST', `/v1/machines/${encodeURIComponent(id)}/stop`)
  }

  start(id: string): Promise<void> {
    return this.http.none('POST', `/v1/machines/${encodeURIComponent(id)}/start`)
  }

  checkpoint(id: string, req: CheckpointRequest = {}): Promise<Checkpoint> {
    return this.http.json<Checkpoint>('POST', `/v1/machines/${encodeURIComponent(id)}/checkpoints`, { body: req })
  }

  listCheckpoints(id: string): Promise<Checkpoint[]> {
    return this.http.json<Checkpoint[]>('GET', `/v1/machines/${encodeURIComponent(id)}/checkpoints`)
  }

  /** Turns a sandbox into a durable service. The URL does not change. */
  promote(id: string, req: PromoteRequest = {}): Promise<Service> {
    return this.http.json<Service>('POST', `/v1/machines/${encodeURIComponent(id)}/promote`, { body: req })
  }

  /**
   * The volume drive Firecracker actually has, not the one hostd meant to set.
   * The difference between the two is a durability guarantee that fails
   * silently, which is why it is reported rather than assumed.
   */
  volume(id: string): Promise<MachineVolume> {
    return this.http.json<MachineVolume>('GET', `/v1/machines/${encodeURIComponent(id)}/volume`)
  }
}

export class Checkpoints {
  private readonly http: Http

  constructor(http: Http) {
    this.http = http
  }

  /**
   * Restores IN PLACE: the same machine, keeping its id, URL and agent token.
   * A restore that created a machine would mint a new URL, which is a bug.
   */
  restore(id: string): Promise<Machine> {
    return this.http.json<Machine>('POST', `/v1/checkpoints/${encodeURIComponent(id)}/restore`)
  }

  /** `durable` flips to true once the upload to object storage lands. */
  get(id: string): Promise<Checkpoint> {
    return this.http.json<Checkpoint>('GET', `/v1/checkpoints/${encodeURIComponent(id)}`)
  }
}

export class Services {
  private readonly http: Http

  constructor(http: Http) {
    this.http = http
  }

  create(req: CreateServiceRequest): Promise<Service> {
    return this.http.json<Service>('POST', '/v1/services', { body: req })
  }

  list(): Promise<Service[]> {
    return this.http.json<Service[]>('GET', '/v1/services')
  }

  get(id: string): Promise<Service> {
    return this.http.json<Service>('GET', `/v1/services/${encodeURIComponent(id)}`)
  }

  deploy(id: string, req: DeployRequest = {}): Promise<Release> {
    return this.http.json<Release>('POST', `/v1/services/${encodeURIComponent(id)}/deploy`, { body: req })
  }

  rollback(id: string): Promise<Release> {
    return this.http.json<Release>('POST', `/v1/services/${encodeURIComponent(id)}/rollback`)
  }

  /**
   * `env` and `secret_env` REPLACE the stored map rather than merging into it,
   * and take effect at the next deploy. `replicas` is reconciled by the
   * autoscaler.
   */
  patch(id: string, req: UpdateServiceRequest): Promise<Service> {
    return this.http.json<Service>('PATCH', `/v1/services/${encodeURIComponent(id)}`, { body: req })
  }

  /** Newest first. */
  releases(id: string): Promise<Release[]> {
    return this.http.json<Release[]>('GET', `/v1/services/${encodeURIComponent(id)}/releases`)
  }
}

export class Domains {
  private readonly http: Http

  constructor(http: Http) {
    this.http = http
  }

  /**
   * 201 when the CNAME already points here, 202 when it does not yet; either
   * way the response names the target the customer's CNAME has to carry.
   */
  add(req: AddDomainRequest): Promise<DomainResponse> {
    return this.http.json<DomainResponse>('POST', '/v1/domains', { body: req })
  }

  list(): Promise<DomainResponse[]> {
    return this.http.json<DomainResponse[]>('GET', '/v1/domains')
  }

  remove(hostname: string): Promise<void> {
    return this.http.none('DELETE', `/v1/domains/${encodeURIComponent(hostname)}`)
  }
}

export class Volumes {
  private readonly http: Http

  constructor(http: Http) {
    this.http = http
  }

  create(req: CreateVolumeRequest): Promise<Volume> {
    return this.http.json<Volume>('POST', '/v1/volumes', { body: req })
  }

  list(): Promise<Volume[]> {
    return this.http.json<Volume[]>('GET', '/v1/volumes')
  }
}

export class Hosts {
  private readonly http: Http

  constructor(http: Http) {
    this.http = http
  }

  /** The fleet as this host sees it, read from its local replica. */
  list(): Promise<Host[]> {
    return this.http.json<Host[]>('GET', '/v1/hosts')
  }
}

export class APIKeys {
  private readonly http: Http

  constructor(http: Http) {
    this.http = http
  }

  /** The plaintext key is in `key`, returned by this call and never again. */
  create(req: CreateAPIKeyRequest): Promise<APIKeyResponse> {
    return this.http.json<APIKeyResponse>('POST', '/v1/api-keys', { body: req })
  }

  revoke(hash: string): Promise<RevokeResponse> {
    return this.http.json<RevokeResponse>('POST', `/v1/api-keys/${encodeURIComponent(hash)}/revoke`)
  }

  list(org: string): Promise<APIKeyResponse[]> {
    return this.http.json<APIKeyResponse[]>('GET', '/v1/api-keys', { query: { org } })
  }
}

export class Quotas {
  private readonly http: Http

  constructor(http: Http) {
    this.http = http
  }

  get(org: string): Promise<QuotaResponse> {
    return this.http.json<QuotaResponse>('GET', `/v1/quotas/${encodeURIComponent(org)}`)
  }

  put(org: string, quota: Omit<QuotaResponse, 'updated_at'>): Promise<QuotaResponse> {
    return this.http.json<QuotaResponse>('PUT', `/v1/quotas/${encodeURIComponent(org)}`, { body: quota })
  }
}

export class Usage {
  private readonly http: Http

  constructor(http: Http) {
    this.http = http
  }

  /** Unix seconds. Defaults to the last 24 hours on the host that answers. */
  get(range: { since?: number; until?: number } = {}): Promise<UsageResponse> {
    return this.http.json<UsageResponse>('GET', '/v1/usage', {
      query: { since: range.since, until: range.until },
    })
  }
}

export class Compose {
  private readonly http: Http

  constructor(http: Http) {
    this.http = http
  }

  /**
   * Plans a compose file into ordered steps. Stateless: nothing is created,
   * and `env` is the interpolation environment the caller chose to send, never
   * the whole process environment.
   */
  plan(req: ComposeRequest): Promise<ComposePlan> {
    return this.http.json<ComposePlan>('POST', '/v1/compose/plan', { body: req })
  }
}
