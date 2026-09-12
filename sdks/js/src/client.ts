/**
 * `PilotsClient`: one method per route, grouped by the noun the route acts on.
 *
 * Every host serves this identical API, so the base URL is any host in the
 * fleet (or the fleet's API name). Nothing here is aware of which host owns a
 * machine: hostd forwards a write that arrived at the wrong host itself.
 */

import { BuildStream } from './build.ts'
import { Http, textLines } from './http.ts'
import type { HttpOptions } from './http.ts'
import { buildExecURL, ExecStream } from './stream.ts'
import type { ExecStreamOptions, WebSocketCtor } from './stream.ts'
import type {
  AddDomainRequest,
  APIKeyResponse,
  Checkpoint,
  CheckpointRequest,
  ComposePlan,
  ComposePlanResponse,
  ComposeRequest,
  ConnectRepoRequest,
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
  RepoLinkListResponse,
  RepoLinkResponse,
  RepoRef,
  ResizeMachineRequest,
  RevokeResponse,
  Service,
  UpdateServiceRequest,
  UsageResponse,
  Volume,
  WhoamiResponse,
} from './types.ts'

export interface ClientOptions extends HttpOptions {
  /** Overrides `globalThis.WebSocket` for every stream this client opens. */
  WebSocket?: WebSocketCtor
}

export class PilotsClient {
  readonly http: Http
  readonly machines: Machines
  readonly builds: Builds
  readonly checkpoints: Checkpoints
  readonly services: Services
  readonly domains: Domains
  readonly volumes: Volumes
  readonly hosts: Hosts
  readonly apiKeys: APIKeys
  readonly repos: Repos
  readonly quotas: Quotas
  readonly usage: Usage
  readonly compose: Compose

  constructor(apiKey: string, opts: ClientOptions = {}) {
    this.http = new Http(apiKey, opts)
    this.machines = new Machines(this.http, opts.WebSocket)
    this.builds = new Builds(this.http)
    this.checkpoints = new Checkpoints(this.http)
    this.services = new Services(this.http)
    this.domains = new Domains(this.http)
    this.volumes = new Volumes(this.http)
    this.hosts = new Hosts(this.http)
    this.apiKeys = new APIKeys(this.http)
    this.repos = new Repos(this.http)
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

  /** The org, scopes and host this client's key resolves to. */
  whoami(): Promise<WhoamiResponse> {
    return this.http.json<WhoamiResponse>('GET', '/v1/whoami')
  }

  /**
   * Asks the host what a directory is, from a tar of it.
   *
   * On the client rather than under `compose` or `services`, because it is
   * the front door: it is what a caller reaches for before it knows whether
   * the directory is a compose project, a Dockerfile or a framework the
   * platform recognises.
   *
   * No client deadline, like a build: the upload is a whole source tree.
   */
  async plan(tar: BodyInit, opts: { app?: string } = {}): Promise<ComposePlanResponse> {
    const res = await this.http.send('POST', '/v1/plan', {
      raw: tar,
      contentType: 'application/x-tar',
      ...(opts.app ? { query: { app: opts.app } } : {}),
      timeoutMs: null,
    })
    return (await res.json()) as ComposePlanResponse
  }

  /**
   * Asks the host what a REPOSITORY is, naming it rather than sending it. The
   * host fetches the ref through the fleet's GitHub App, the same path a push
   * takes.
   *
   * For a caller that holds no repository bytes: a browser session holds an
   * App JWT and nothing else. A fleet with no App configured answers
   * `not_configured` and says to send a tar instead.
   */
  async planRepo(ref: RepoRef, opts: { app?: string } = {}): Promise<ComposePlanResponse> {
    const res = await this.http.send('POST', '/v1/plan', {
      body: ref,
      ...(opts.app ? { query: { app: opts.app } } : {}),
      timeoutMs: null,
    })
    return (await res.json()) as ComposePlanResponse
  }
}

export class Machines {
  private readonly http: Http
  private readonly WebSocket: WebSocketCtor | undefined

  constructor(http: Http, ws?: WebSocketCtor) {
    this.http = http
    this.WebSocket = ws
  }

  /**
   * No client deadline. A create from the golden template is sub-second, but a
   * create from a BUILD is a kernel boot -- twenty seconds and up, and more
   * with a volume to mount -- and an abort at thirty seconds leaves a machine
   * running that the caller has no id for. See Services.deploy.
   */
  create(req: CreateMachineRequest = {}): Promise<Machine> {
    return this.http.json<Machine>('POST', '/v1/machines', { body: req, timeoutMs: null })
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

  /**
   * Buffered exec. For output nobody wants to hold in memory, use execStream.
   *
   * A `timeout_ms` longer than the client's own deadline extends it, with a
   * margin: otherwise the client would abort a command the server was still
   * willing to run, and the caller would get a network error instead of the
   * server's own timeout result.
   */
  exec(id: string, req: ExecRequest): Promise<ExecResponse> {
    const timeoutMs = req.timeout_ms ? req.timeout_ms + 5_000 : undefined
    return this.http.json<ExecResponse>('POST', `/v1/machines/${encodeURIComponent(id)}/exec`, {
      body: req,
      ...(timeoutMs !== undefined && timeoutMs > this.http.timeoutMs ? { timeoutMs } : {}),
    })
  }

  logs(id: string): Promise<string> {
    return this.http.text('GET', `/v1/machines/${encodeURIComponent(id)}/logs`)
  }

  /**
   * Streams a command's output frame by frame. `stdin` is false by default;
   * see ExecStream before turning it on.
   *
   * `tty: true` runs the command on a pseudo-terminal, which is what an
   * interactive shell needs; it implies `stdin` and merges stderr into stdout.
   */
  execStream(id: string, argv: string[], opts: ExecStreamOptions = {}): ExecStream {
    const url = buildExecURL(
      this.http.baseURL,
      `/v1/machines/${encodeURIComponent(id)}/exec/stream`,
      argv,
      opts,
      this.http.org,
    )
    return new ExecStream(url, this.http.apiKey, {
      // A tty implies stdin, so the pair is settled here rather than left to
      // each caller: hostd refuses tty=true with stdin=false outright.
      stdin: opts.tty ? true : (opts.stdin ?? false),
      tty: opts.tty ?? false,
      ...(opts.WebSocket ?? this.WebSocket ? { WebSocket: opts.WebSocket ?? this.WebSocket! } : {}),
    })
  }

  /** Follows the console log line by line. Never given a client deadline. */
  async *followLogs(id: string): AsyncGenerator<string, void, undefined> {
    const res = await this.http.send('GET', `/v1/machines/${encodeURIComponent(id)}/logs`, {
      query: { follow: '1' },
      timeoutMs: null,
    })
    yield* textLines(res)
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

  /**
   * Boots a machine again at a new size, in place: same id, same URL, same
   * disk, same volume. Omit a dimension to leave it alone.
   *
   * A boot rather than a resume, because a memory image cannot be loaded into
   * a differently-sized VM, so the machine loses what was in memory.
   */
  resize(id: string, req: ResizeMachineRequest): Promise<Machine> {
    return this.http.json<Machine>('POST', `/v1/machines/${encodeURIComponent(id)}/resize`, { body: req })
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

export class Builds {
  private readonly http: Http

  constructor(http: Http) {
    this.http = http
  }

  /**
   * Uploads a build context (a tar) and streams the build's NDJSON log.
   *
   * No client deadline: the whole point of the stream is that the build
   * outlives the request that started it.
   *
   * `deploy` names a service to cut a release for from the image. The HOST
   * does that, on the verdict, exactly once -- so a caller that walks away
   * mid-build still ends with a release, and two callers watching one build
   * still produce one rollout. See `BuildLogLine.release`.
   */
  async create(tar: BodyInit, opts: { deploy?: string } = {}): Promise<BuildStream> {
    const res = await this.http.send('POST', '/v1/builds', {
      raw: tar,
      contentType: 'application/x-tar',
      ...(opts.deploy ? { query: { deploy: opts.deploy } } : {}),
      timeoutMs: null,
    })
    return new BuildStream(res)
  }

  /**
   * Builds a REPOSITORY, naming it rather than uploading it. The host fetches
   * the ref through the fleet's GitHub App, plans it, and builds the one step
   * a plan may produce, which is the path a push already takes.
   *
   * The stream is the one `create` returns, so a caller reads the verdict the
   * same way. A plan with more than one step is refused with
   * `plan_multi_service`, readable at the build's own log. `deploy` is what
   * `create` takes it for.
   */
  async createFromRepo(ref: RepoRef, opts: { app?: string; deploy?: string } = {}): Promise<BuildStream> {
    const query: Record<string, string> = {}
    if (opts.app) query.app = opts.app
    if (opts.deploy) query.deploy = opts.deploy
    const res = await this.http.send('POST', '/v1/builds', {
      body: ref,
      ...(Object.keys(query).length > 0 ? { query } : {}),
      timeoutMs: null,
    })
    return new BuildStream(res)
  }

  /** Replays a build's log, following it live when asked. */
  async logs(id: string, opts: { follow?: boolean } = {}): Promise<BuildStream> {
    const res = await this.http.send('GET', `/v1/builds/${encodeURIComponent(id)}/logs`, {
      ...(opts.follow ? { query: { follow: '1' } } : {}),
      timeoutMs: null,
    })
    return new BuildStream(res, id)
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

  /**
   * NO client deadline, for the reason a build stream has none: a rollout
   * takes as long as the release takes to prove itself. It boots a replica,
   * gates it for up to the health check's grace period -- which a compose
   * `start_period` routinely sets to minutes -- snapshots it, and restores
   * every other replica from that snapshot.
   *
   * The 30-second default aborted every deploy slower than that, and the
   * abort was not merely a bad message. It cancelled the request context the
   * rollout was running on, which cancelled the health gate AND the cleanup
   * that runs when the gate fails, so the machine stayed up carrying the new
   * release while the service row never moved to it -- a service whose
   * release_id was "" and which had no URL, and a client told only "The
   * operation was aborted due to timeout". The server side of that is fixed
   * too; a client that hangs up mid-rollout must not be the normal case.
   */
  deploy(id: string, req: DeployRequest = {}): Promise<Release> {
    return this.http.json<Release>('POST', `/v1/services/${encodeURIComponent(id)}/deploy`, {
      body: req,
      timeoutMs: null,
    })
  }

  /** No client deadline: a rollback is a rollout. See deploy. */
  rollback(id: string): Promise<Release> {
    return this.http.json<Release>('POST', `/v1/services/${encodeURIComponent(id)}/rollback`, {
      timeoutMs: null,
    })
  }

  /**
   * `env`, `secret_env` and `replicas` REPLACE the stored values rather than
   * merging into them, and all three take effect at the next deploy, which is
   * where a rollout reads them.
   *
   * `knobs` are refused here with a 400 naming the field: a service row has no
   * knobs column and a replica row is single-writer to its own host, so they
   * travel on `deploy` instead.
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

/**
 * Which repositories an org may have the fleet fetch.
 *
 * The record `POST /v1/builds` and `POST /v1/plan` consult before fetching a
 * repository by name. `connect` needs an admin-scoped key -- the proof that an
 * org controls a repository is held at GitHub, not in a request -- while
 * `list` and the build itself need only the org's own key.
 */
export class Repos {
  private readonly http: Http

  constructor(http: Http) {
    this.http = http
  }

  /** Idempotent: the row is write-once, so connecting twice is one connection. */
  connect(repo: string): Promise<RepoLinkResponse> {
    return this.http.json<RepoLinkResponse>('POST', '/v1/repos', { body: { repo } satisfies ConnectRepoRequest })
  }

  async list(): Promise<RepoLinkResponse[]> {
    const res = await this.http.json<RepoLinkListResponse>('GET', '/v1/repos')
    return res.repos ?? []
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
