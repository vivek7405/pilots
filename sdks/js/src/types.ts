/**
 * The wire contract, mirrored from `apps/hostd/internal/api` and
 * `apps/hostd/internal/compose`.
 *
 * One interface per hostd struct, same name, properties in the JSON tag's
 * snake_case and optional exactly where the tag carries `omitempty`. Structs
 * from the `compose` package are mirrored with a `Compose` prefix, because
 * `Plan`, `Step` and `Request` are far too generic to export unqualified.
 *
 * `test/drift.test.ts` parses hostd's Go source on every run and fails naming
 * the struct and the tag when the two sides disagree, in either direction. Do
 * not edit these shapes to match a server you are guessing at: edit them to
 * match the Go, and let the test say when that is done.
 */

/**
 * A machine's lifecycle state (`Machine.state`).
 *
 * The six hostd writes, from `internal/state/schema.sql`. `destroyed` is one
 * of them and is NOT a deleted row: a delete racing a replica that still
 * carries the insert loses through the merge and the machine comes back, so a
 * destroyed machine is a tombstone with a state. A client listing machines
 * therefore has to filter it out itself.
 */
export type MachineState = 'creating' | 'running' | 'suspended' | 'stopped' | 'error' | 'destroyed'

/** Per-machine lifecycle policy. A sandbox and a service differ only here. */
export interface Knobs {
  auto_stop: 'off' | 'suspend'
  auto_start: boolean
  min_machines_running: number
  soft_limit: number
  /** Seconds of quiet before the machine suspends, 1..3600 (default 60). */
  idle_timeout: number
  /**
   * The machine's cron jobs; `null` means none. On a deploy an absent key
   * inherits the previous release's and an explicit `[]` clears them.
   */
  schedules: Schedule[] | null
}

/**
 * One cron job: a five-field expression (UTC; or `@hourly`, `@daily`,
 * `@weekly`, `@monthly`) and exactly one of a path the host GETs on the
 * machine or a command it runs in it. A GET carries the `X-Pilot-Cron`
 * header, which cannot arrive from outside the fleet.
 */
export interface Schedule {
  cron: string
  path?: string
  cmd?: string
}

/**
 * A PARTIAL lifecycle policy: the shape a REQUEST carries.
 *
 * hostd decodes a request's knobs onto a value it has already seeded -- its
 * own defaults on a create, the sibling replica's policy on a deploy -- so a
 * key left out keeps the value it had. A full `Knobs` here would force the
 * caller to spell all four, and the three they did not care about would be
 * merged as zeros: `auto_start: false` suspends the replica after a minute
 * and the router then refuses to wake it, which is a permanently dead URL
 * earned by raising a concurrency limit.
 *
 * Omit a key to leave it alone; send it to set it, zero values included, so
 * `min_machines_running: 0` (scale to zero) stays sayable.
 */
export type KnobsPatch = Partial<Knobs>

/** The platform's one primitive. Its id, name and URL never change. */
export interface Machine {
  /**
   * The org that owns this object. Present when the caller is an admin key,
   * which is the only caller that sees objects across orgs; a tenant-scoped
   * key only ever sees its own, so the field carries nothing it did not
   * already know.
   */
  org_id?: string
  id: string
  name: string
  host_id: string
  state: MachineState
  knobs: Knobs
  image_ref?: string
  vcpus: number
  mem_mib: number
  url: string
  custom_domain?: string
  volume_id?: string
  service_id?: string
  release_id?: string
  app?: string
  created_at: number
  last_activity: number
  /**
   * How this machine last came up. `restore` resumed its memory image;
   * `boot` is the kernel boot a create with an image or a volume pays once,
   * and every redeploy; `cold_boot` is a restore DOWNGRADED because no host
   * of the image's CPU vendor was alive. A cold boot keeps the id, name, URL,
   * disk and volume, and loses processes and everything in memory.
   */
  last_start?: 'restore' | 'boot' | 'cold_boot'
  last_start_at?: number
  /** Labels attached at create, for finding the machine again. */
  labels?: Record<string, string>
  /** Who may reach the URL: 'public' (the default) or 'org'. */
  url_auth?: 'public' | 'org'
}

/** PATCH /v1/machines/{id}: who may reach the URL. */
export interface UpdateMachineRequest {
  url_auth?: 'public' | 'org'
}

export interface CreateMachineRequest {
  name?: string
  image?: string
  template?: string
  checkpoint?: string
  vcpus?: number
  mem_mib?: number
  /** A patch: what is present merges onto hostd's defaults, the rest stay. */
  knobs?: KnobsPatch
  volume?: string
  app?: string
  cmd?: string
  mem_build_id?: string
  rootfs_build_id?: string
  service?: string
  release?: string
  env?: Record<string, string>
  secret_env?: Record<string, string>
  labels?: Record<string, string>
  /** Who may reach the URL: 'public' (the default) or 'org'. */
  url_auth?: 'public' | 'org'
}

export interface ExecRequest {
  cmd: string
  cwd?: string
  env?: Record<string, string>
  user?: string
  timeout_ms?: number
}

export interface ExecResponse {
  stdout: string
  stderr: string
  exit_code: number
}

export interface CheckpointRequest {
  comment?: string
}

export interface Checkpoint {
  id: string
  machine_id: string
  seq: number
  comment?: string
  source_id?: string
  durable: boolean
  created_at: number
  /** Present only on the response that created the checkpoint. */
  resume_gap_ms?: number
}

export interface BuildLogLine {
  step?: string
  stream?: string
  line?: string
  ts: number
  error?: string
  /** The rootfs build id, on the last line of a successful build. */
  result?: string
  /** The stable code on a terminal failure line, `build_failed`. */
  code?: string
  /**
   * The deployment cut from this image, on the last line of a build whose
   * request named a service to deploy (`{ deploy }`). Its presence is what
   * says the release exists: `result` says only that the image does.
   */
  release?: string
  /**
   * The reader's next step, on a terminal failure line. A deploy refused
   * after the image was built -- a health gate that never passed, above all
   * -- reaches the reader through the log, so it carries the same `next` the
   * deploy route would have answered with.
   */
  next?: string
}

export interface HealthCheck {
  type?: string
  path?: string
  test?: string[]
  interval?: number
  timeout?: number
  grace?: number
  healthy_threshold?: number
}

export interface Service {
  /**
   * The org that owns this object. Present when the caller is an admin key,
   * which is the only caller that sees objects across orgs; a tenant-scoped
   * key only ever sees its own, so the field carries nothing it did not
   * already know.
   */
  org_id?: string
  id: string
  name: string
  app?: string
  /**
   * The sibling services in this app whose `<name>.internal` address this
   * service's environment references. Derived by hostd on every read from
   * both halves of the environment and stored nowhere, so it says what the
   * service is configured to dial right now.
   */
  depends_on?: string[]
  replicas: number
  knobs: Knobs
  health?: HealthCheck
  url?: string
  custom_domain?: string
  release_id?: string
  /**
   * The volume every replica of this service mounts. A service with one runs
   * one replica, because a volume is mounted by exactly one machine.
   */
  volume_id?: string
  repo?: string
  branch?: string
  autodeploy: boolean
  created_at: number
  /** Labels attached at create, or copied from the machine promote made it from. */
  labels?: Record<string, string>
  /** Who may reach the URL: 'public' (the default) or 'org'. */
  url_auth?: 'public' | 'org'
}

/**
 * The body `POST /v1/plan` and `POST /v1/builds` accept in place of a tar: a
 * repository the fleet's GitHub App is installed on, at a ref. The host
 * fetches the bytes itself, through the path a push takes, so no client has to
 * hold them.
 */
export interface RepoRef {
  /** owner/name */
  repo: string
  /** branch, tag or sha */
  ref: string
}

export interface CreateServiceRequest {
  name: string
  app?: string
  release?: string
  build?: string
  replicas?: number
  /**
   * Accepted for wire compatibility and not persisted: a service row keeps no
   * knobs, so a policy set here goes nowhere and the deploy is where it
   * belongs.
   */
  knobs?: KnobsPatch
  health?: HealthCheck
  /**
   * The subdomain label under the fleet's domain. Empty means one is minted
   * from the name: the name itself when it is free, else the name and a
   * four-character suffix. Set it to ask for an exact label, which is taken
   * literally or refused, never adjusted.
   */
  domain?: string
  /**
   * Mints no address at all. The service is reachable by peers over
   * <name>.internal, and its replicas keep their own machine URLs the way
   * every machine does. Create-only: an address, once minted, is permanent.
   */
  private?: boolean
  custom_domain?: string
  /**
   * Create-only: a volume swap is a data migration, not a configuration
   * change, so the update route does not take it. Requires replicas of at
   * most one.
   */
  volume?: string
  env?: Record<string, string>
  secret_env?: Record<string, string>
  /** Labels attached at create, for finding it again. */
  labels?: Record<string, string>
  /** Who may reach the URL: 'public' (the default) or 'org'. */
  url_auth?: 'public' | 'org'
  repo?: string
  branch?: string
  autodeploy?: boolean
}

export interface DeployRequest {
  release?: string
  build?: string
  /**
   * Lifecycle policy for the replicas this deploy creates, merged onto what
   * the previous release's replicas carry. A service row keeps no knobs, so
   * the deploy is where they travel.
   *
   * A patch, so raising one field does not zero the three nobody mentioned.
   */
  knobs?: KnobsPatch
}

export interface PromoteRequest {
  custom_domain?: string
  replicas?: number
  health?: HealthCheck
}

/**
 * Boots a machine again from another image, in place: same row, same URL,
 * same volume. How a volume-backed service takes a release. Sent by the
 * rollout inside the fleet; there is no client method for it.
 */
export interface RedeployRequest {
  image: string
  release?: string
}

export interface Release {
  id: string
  service_id: string
  rootfs_build_id?: string
  mem_build_id?: string
  healthy: boolean
  created_at: number
}

export interface Volume {
  /**
   * The org that owns this object. Present when the caller is an admin key,
   * which is the only caller that sees objects across orgs; a tenant-scoped
   * key only ever sees its own, so the field carries nothing it did not
   * already know.
   */
  org_id?: string
  id: string
  name: string
  size_gib: number
  machine_id?: string
  host_id?: string
  mount_path: string
  created_at: number
}

export interface MachineVolume {
  volume_id: string
  mount_path: string
  device: string
  cache_type: string
}

export interface CreateVolumeRequest {
  name: string
  size_gib: number
  mount_path?: string
}

export interface Host {
  id: string
  public_ip?: string
  wg_addr?: string
  cpu_free: number
  mem_free_mib: number
  last_seen: number
  alive: boolean
  /**
   * Which vendor pool this host restores memory images from, the raw
   * /proc/cpuinfo vendor_id. Absent on a host that has not published it yet.
   */
  cpu_vendor?: string
}

/**
 * What the caller's key resolves to on the host that answered.
 *
 * `org_id` is empty for a key that belongs to no org, which is the bootstrap
 * admin key's case, so a caller that needs to distinguish "no org" from "not
 * asked" checks for the empty string rather than for the field.
 */
export interface WhoamiResponse {
  org_id: string
  scopes: string[]
  host_id: string
}

export interface AddDomainRequest {
  service_id: string
  hostname: string
}

export interface DomainResponse {
  hostname: string
  service_id: string
  verified: boolean
  cname_target: string
  created_at: number
}

export interface HealthResponse {
  ok: boolean
  host_id: string
  reflink: boolean
  /**
   * Whether guest memory on this host is backed by 2MiB pages. Unlike
   * reflink this is not only a speed signal: the page size is recorded in
   * every snapshot and cannot be reinterpreted at restore, so a host that
   * disagrees with the fleet cannot restore the fleet's machines at all.
   */
  hugepages: boolean
  /**
   * The sum of the local replica's version vector: how many changes, from
   * every host, this replica has applied. 0 on a single-box SQLite host.
   * Comparable across hosts, so two hosts far apart on this number are a
   * replication problem before they are anything else.
   */
  store_version: number
  /**
   * This host's CPU vendor pool, the raw /proc/cpuinfo vendor_id. A memory
   * image never restores across the Intel/AMD boundary, so this says which
   * of the fleet's snapshots this host can load.
   */
  cpu_vendor: string
  /** True only when a fault flag is making this host lie about its CPU. */
  cpu_vendor_forced?: boolean
  /**
   * The same number broken out per actor: how far this replica has applied
   * each host's changes, keyed by site id in hex. The sum answers "are we far
   * apart"; this answers "on whose rows". Empty on a single-box SQLite host.
   */
  store_versions?: Record<string, number>
  /**
   * False while this host is still catching up with the fleet. Such a host
   * serves its own machines normally and claims none of anybody else's, so it
   * is healthy, not broken. Stuck false for more than a few seconds is a
   * replication problem.
   */
  replication_complete: boolean
}

export interface ErrorResponse {
  error: string
  /** A stable snake_case noun to branch on. See `internal/api/errors.go`. */
  code?: string
  /** The one thing to do about it, naming the command or the call. */
  next?: string
  /** Typed per code: `HealthGateDetails`, `ComposeUnknownDetails`. */
  details?: unknown
}

/**
 * The 422 `health_gate_failed` body's `details`, and the reason a release was
 * refused. It carries no address on purpose: the probe target is the host's
 * own view of the replica and is not reachable from where this is read.
 */
export interface HealthGateDetails {
  service: string
  replica: string
  release: string
  grace_sec: number
  last: HealthLast
}

/** The replica's last answer: a status and body, or why it said nothing. */
export interface HealthLast {
  status?: number
  body?: string
  error?: string
}

// ---------------------------------------------------------------------------
// The data routes: the service patch, the usage ledger and the compose plan.
// hostd serves all three, and the drift test checks every shape below against
// `internal/api` and `internal/compose` on each run.
// ---------------------------------------------------------------------------

export interface UpdateServiceRequest {
  replicas?: number
  health?: HealthCheck
  env?: Record<string, string>
  secret_env?: Record<string, string>
  repo?: string
  branch?: string
  autodeploy?: boolean
  /**
   * Gives an address to a service that has none, which is the only way one
   * created before addresses were minted, or one created private, can get
   * one. Accepted exactly once: a service that already has an address is a
   * 409 and an empty string a 400, because URLs are permanent.
   */
  domain?: string
  /** Who may reach the address: 'public' or 'org'. */
  url_auth?: 'public' | 'org'
}

export interface CreateAPIKeyRequest {
  org_id: string
  scopes: string[]
  /**
   * The three RESTRICTIONS, all optional and all enforced by the fleet.
   * `name_prefix` is what every machine and service the key names must start
   * with, `max_machines` caps how many of them may exist at once, and
   * `expires_at` (unix seconds) is when the key stops authenticating. Absent
   * means unrestricted, which is what an operator's own key is.
   *
   * Write-once with the key: a restriction that could be widened later would
   * not be one, so a narrower key is minted again rather than edited.
   */
  name_prefix?: string
  max_machines?: number
  expires_at?: number
}

export interface APIKeyResponse {
  /** The key itself, returned once, by the call that minted it. */
  key?: string
  hash: string
  org_id: string
  scopes: string[]
  created_at: number
  revoked_at?: number
  /**
   * The restrictions this key carries, echoed on the mint and on every
   * listing. Absent means unrestricted.
   */
  name_prefix?: string
  max_machines?: number
  expires_at?: number
}

export interface RevokeResponse {
  hash: string
  revoked_at: number
}

/**
 * Tie a repository to an org, which is what lets that org's own keys name it
 * in a `{repo, ref}` build or plan.
 *
 * Admin-scoped: the proof that an org controls a repository is held at GitHub,
 * so the connection is asserted once by a party that can prove it. The org is
 * not in the body -- it comes from the key, or from `?org=` on an admin key,
 * as it does on every other create.
 */
export interface ConnectRepoRequest {
  repo: string
}

/** One connection between an org and a repository. */
export interface RepoLinkResponse {
  repo: string
  org_id: string
  connected_at: number
}

export interface RepoLinkListResponse {
  repos: RepoLinkResponse[]
}

export interface QuotaResponse {
  org_id: string
  max_machines: number
  max_vcpus: number
  max_mem_mib: number
  max_volume_gib: number
  max_builds: number
  /**
   * How much object storage this org's checkpoints may hold. Zero on a PUT
   * means the default rather than none, so a client written against the older
   * body shape does not freeze an org's checkpoints by omitting it.
   */
  max_snapshot_gib: number
  updated_at: number
}

export interface QuotaExceededResponse {
  error: string
  code: string
  next: string
  quota: string
  limit: number
  used: number
  /** "host" on a build, which is rate-limited per host rather than per org. */
  scope?: string
}

export interface UsageTotals {
  machine_seconds: number
  vcpu_seconds: number
  mib_seconds: number
  volume_gib_seconds: number
  /**
   * What this org's checkpoints held in object storage, accrued in EVERY
   * machine state: the bytes are there whatever the guest is doing, which is
   * why a stopped machine is not free.
   */
  snapshot_gib_seconds: number
}

export interface UsageResponse {
  host_id: string
  since: number
  until: number
  orgs: Record<string, UsageTotals>
  /**
   * The same accrual per machine, keyed by org and then by machine id. Present
   * only for `by=machine`, because it is the larger answer and most callers
   * want the invoice line rather than its derivation.
   */
  machines?: Record<string, Record<string, UsageTotals>>
}

// ---------------------------------------------------------------------------
// `internal/compose`, mirrored under a Compose prefix.
// ---------------------------------------------------------------------------

export interface ComposeRequest {
  /** The compose file's text, not a path. */
  compose: string
  /** The interpolation environment for `${VAR}`. */
  env?: Record<string, string>
}

export interface ComposeBuild {
  context?: string
  dockerfile?: string
}

export interface ComposeVolume {
  name: string
  size_gib: number
  mount_path: string
}

export interface ComposeStep {
  name: string
  build?: ComposeBuild
  dockerfile?: string
  /**
   * What the compose file overrode - command:, entrypoint:, working_dir:,
   * user: - rendered as Dockerfile instructions to append to the build
   * context's own Dockerfile before uploading it.
   */
  dockerfile_append?: string
  env?: Record<string, string>
  secret_refs?: Record<string, string>
  ports?: number[]
  health?: HealthCheck
  volumes?: ComposeVolume[]
  replicas: number
  vcpus: number
  mem_mib: number
  depends_on?: string[]
  /**
   * A patch: a step's knobs are whatever the compose file spelled out, and
   * they are spread straight onto a DeployRequest, so a key the file left out
   * must stay absent rather than arrive as a zero.
   */
  knobs?: KnobsPatch
  domain?: string
  /**
   * Asks for no address at all. A service without it is given one from its
   * name, so this is how a database says it has nothing to serve.
   */
  private?: boolean
  custom_domain?: string
  pre_deploy?: string
  /**
   * Filled when SEVERAL compose services share one build context and therefore
   * run as one machine with one process each. Absent is the ordinary case: one
   * service, one machine, one process named `app`.
   */
  processes?: ComposeProcess[]
}

/** One named command inside a machine that runs several. */
export interface ComposeProcess {
  name: string
  cmd?: string
  /** Everything named here starts before this process does. */
  needs?: string[]
  /** Marks the one process that owns the machine's published port. */
  port?: boolean
}

export interface ComposePlan {
  app: string
  steps: ComposeStep[]
}

export interface ComposeUnsupported {
  service: string
  key: string
  message: string
}

export interface ComposePlanError {
  error: string
  code: string
  next: string
  unsupported: ComposeUnsupported[]
}

/** `POST /v1/plan`'s 200 body: the plan, and how each step was decided. */
export interface ComposePlanResponse {
  plan: ComposePlan
  detected: ComposeDetected[]
}

/**
 * Where one step came from. `source` is "compose", "dockerfile" or "recipe";
 * `framework` and `notes` are set for a recipe only.
 */
export interface ComposeDetected {
  service: string
  source: string
  framework?: string
  dir: string
  port: number
  health?: HealthCheck
  notes?: string[]
}

/**
 * The 400 `unknown_framework`'s `details`: everything needed to write the
 * Dockerfile by hand, so the refusal is a starting point and not a dead end.
 */
export interface ComposeUnknownDetails {
  dir: string
  looked_for: string[]
  listing: string[]
  manifests?: Record<string, string>
  workspaces?: string[]
  /** The two lines every Dockerfile must obey. */
  rules: string[]
}

// ---------------------------------------------------------------------------
// Exec stream frame prefixes.
//
// Byte-compatible with the sprites protocol, which is why an existing sprites
// client drops in unchanged. Mirrors the `FrameXxx` constants in
// `internal/api/types.go`; the drift test compares the numbers.
// ---------------------------------------------------------------------------

export const FrameStdin = 0
export const FrameStdout = 1
export const FrameStderr = 2
export const FrameExit = 3
export const FrameStdinEOF = 4
