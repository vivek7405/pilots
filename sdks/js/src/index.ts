/**
 * `@pilots/sdk` -- the typed client for the pilots API.
 *
 *     import { PilotsClient } from '@pilots/sdk'
 *     const pilots = new PilotsClient(process.env.PILOT_API_KEY!)
 *     const m = await pilots.machines.create({ name: 'demo' })
 *
 * The sprites-compatible adapter lives at `@pilots/sdk/sprites-compat`.
 *
 * Exports are listed rather than star-exported on purpose: `ComposePlanError`
 * names both a wire shape (in types.ts, where the drift test checks it) and
 * the error class a caller catches, and only one of the two can carry the
 * name here. The class wins, because it satisfies the interface.
 */

export { PilotsClient } from './client.ts'
export type { ClientOptions } from './client.ts'
export {
  APIKeys,
  Builds,
  Checkpoints,
  Compose,
  Domains,
  Hosts,
  Machines,
  Quotas,
  Services,
  Usage,
  Volumes,
} from './client.ts'

export { Http, ndjson, textLines, resolveBaseURL, DEFAULT_BASE_URL, DEFAULT_TIMEOUT_MS } from './http.ts'
export type { FetchLike, HttpOptions } from './http.ts'

export { BuildStream } from './build.ts'
export { buildExecURL, ExecStream } from './stream.ts'
export type { ExecStreamInit, ExecStreamOptions, WebSocketCtor } from './stream.ts'

export {
  BuildFailedError,
  ComposePlanError,
  NotFoundError,
  PilotsError,
  QuotaExceededError,
} from './errors.ts'
export type { PilotsErrorInit } from './errors.ts'

export type {
  AddDomainRequest,
  APIKeyResponse,
  BuildLogLine,
  Checkpoint,
  CheckpointRequest,
  ComposeBuild,
  ComposePlan,
  ComposeRequest,
  ComposeStep,
  ComposeUnsupported,
  ComposeVolume,
  CreateAPIKeyRequest,
  CreateMachineRequest,
  CreateServiceRequest,
  CreateVolumeRequest,
  DeployRequest,
  DomainResponse,
  ErrorResponse,
  ExecRequest,
  ExecResponse,
  HealthCheck,
  HealthResponse,
  Host,
  Knobs,
  Machine,
  MachineState,
  MachineVolume,
  PromoteRequest,
  QuotaExceededResponse,
  QuotaResponse,
  Release,
  RevokeResponse,
  Service,
  UpdateServiceRequest,
  UsageResponse,
  UsageTotals,
  Volume,
} from './types.ts'

export { FrameExit, FrameStderr, FrameStdin, FrameStdinEOF, FrameStdout } from './types.ts'
