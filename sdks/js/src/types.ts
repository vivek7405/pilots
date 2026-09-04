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

/** A machine's lifecycle state (`Machine.state`). */
export type MachineState = 'creating' | 'running' | 'suspended' | 'stopped' | 'error'

/** Per-machine lifecycle policy. A sandbox and a service differ only here. */
export interface Knobs {
  auto_stop: 'off' | 'stop' | 'suspend'
  auto_start: boolean
  min_machines_running: number
  soft_limit: number
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
  replicas: number
  knobs: Knobs
  health?: HealthCheck
  url?: string
  custom_domain?: string
  release_id?: string
  repo?: string
  branch?: string
  autodeploy: boolean
  created_at: number
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
  domain?: string
  custom_domain?: string
  env?: Record<string, string>
  secret_env?: Record<string, string>
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
}

export interface ErrorResponse {
  error: string
}

// ---------------------------------------------------------------------------
// Shapes hostd grows in #30. They are mirrored here now so the client methods
// that call those routes are written once; the drift test starts checking them
// the day the structs land in `internal/api`.
// ---------------------------------------------------------------------------

export interface UpdateServiceRequest {
  replicas?: number
  health?: HealthCheck
  env?: Record<string, string>
  secret_env?: Record<string, string>
  repo?: string
  branch?: string
  autodeploy?: boolean
}

export interface CreateAPIKeyRequest {
  org_id: string
  scopes: string[]
}

export interface APIKeyResponse {
  /** The key itself, returned once, by the call that minted it. */
  key?: string
  hash: string
  org_id: string
  scopes: string[]
  created_at: number
  revoked_at?: number
}

export interface RevokeResponse {
  hash: string
  revoked_at: number
}

export interface QuotaResponse {
  org_id: string
  max_machines: number
  max_vcpus: number
  max_mem_mib: number
  max_volume_gib: number
  max_builds: number
  updated_at: number
}

export interface QuotaExceededResponse {
  error: string
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
}

export interface UsageResponse {
  host_id: string
  since: number
  until: number
  orgs: Record<string, UsageTotals>
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
  cmd?: string
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
  custom_domain?: string
  pre_deploy?: string
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
  unsupported: ComposeUnsupported[]
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
