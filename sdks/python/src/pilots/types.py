"""The wire contract, mirrored from ``apps/hostd/internal/api`` and
``apps/hostd/internal/compose``.

One dataclass per hostd struct, same name, fields named by the JSON tag and
optional exactly where the tag carries ``omitempty``. Structs from the
``compose`` package carry a ``Compose`` prefix, because ``Plan``, ``Step`` and
``Request`` are too generic to export unqualified.

``tests/test_drift.py`` parses hostd's Go source on every run and fails naming
the struct and the tag when the two sides disagree, in either direction. Do
not edit these shapes to match a server you are guessing at: edit them to
match the Go, and let the test say when that is done.

Every field has a default so a response from a newer or older host decodes
without raising; the type hints say which fields hostd always sends.
"""

from __future__ import annotations

import dataclasses
import typing
from dataclasses import dataclass, field
from typing import Any, Literal, TypeVar, get_args, get_origin, get_type_hints

T = TypeVar("T")

#: The six states hostd writes. ``destroyed`` is a tombstone rather than a
#: deleted row, so a client listing machines filters it out itself.
MachineState = Literal["creating", "running", "suspended", "stopped", "error", "destroyed"]
URLAuth = Literal["public", "org"]

#: A PARTIAL lifecycle policy: the shape a REQUEST carries. Omit a key to
#: leave it alone; send it to set it, zero values included.
KnobsPatch = dict[str, Any]


@dataclass
class Schedule:
    """One cron job on a machine.

    `cron` is five fields in UTC, or one of @hourly/@daily/@weekly/@monthly.
    Exactly one of `path` (GET it on the machine) or `cmd` (run it inside)
    carries the work; the machine is woken for either.
    """

    cron: str = ""
    path: str = ""
    cmd: str = ""


@dataclass
class Knobs:
    """Per-machine lifecycle policy. A sandbox and a service differ only here."""

    auto_stop: Literal["off", "stop", "suspend"] = "off"
    auto_start: bool = False
    min_machines_running: int = 0
    soft_limit: int = 0
    hard_limit: int = 0
    idle_timeout: int = 0
    schedules: list[Schedule] = field(default_factory=list)


@dataclass
class Machine:
    """The platform's one primitive. Its id, name and URL never change."""

    org_id: str | None = None
    id: str = ""
    name: str = ""
    host_id: str = ""
    state: str = ""
    knobs: Knobs = field(default_factory=Knobs)
    image_ref: str | None = None
    vcpus: int = 0
    mem_mib: int = 0
    url: str = ""
    custom_domain: str | None = None
    volume_id: str | None = None
    service_id: str | None = None
    release_id: str | None = None
    app: str | None = None
    created_at: int = 0
    last_activity: int = 0
    #: ``restore``, ``boot`` or ``cold_boot``: how this machine last came up.
    last_start: str | None = None
    last_start_at: int | None = None
    labels: dict[str, str] | None = None
    url_auth: str | None = None


@dataclass
class UpdateMachineRequest:
    url_auth: str | None = None


@dataclass
class CreateMachineRequest:
    name: str | None = None
    image: str | None = None
    template: str | None = None
    checkpoint: str | None = None
    vcpus: int | None = None
    mem_mib: int | None = None
    knobs: KnobsPatch | None = None
    volume: str | None = None
    app: str | None = None
    cmd: str | None = None
    mem_build_id: str | None = None
    rootfs_build_id: str | None = None
    service: str | None = None
    release: str | None = None
    env: dict[str, str] | None = None
    secret_env: dict[str, str] | None = None
    labels: dict[str, str] | None = None
    url_auth: str | None = None


@dataclass
class ExecRequest:
    cmd: str = ""
    cwd: str | None = None
    env: dict[str, str] | None = None
    user: str | None = None
    timeout_ms: int | None = None


@dataclass
class ExecResponse:
    stdout: str = ""
    stderr: str = ""
    exit_code: int = 0


@dataclass
class CheckpointRequest:
    comment: str | None = None


@dataclass
class Checkpoint:
    id: str = ""
    machine_id: str = ""
    seq: int = 0
    comment: str | None = None
    source_id: str | None = None
    durable: bool = False
    created_at: int = 0
    #: Present only on the response that created the checkpoint.
    resume_gap_ms: int | None = None


@dataclass
class BuildLogLine:
    step: str | None = None
    stream: str | None = None
    line: str | None = None
    ts: int = 0
    error: str | None = None
    #: The rootfs build id, on the last line of a successful build.
    result: str | None = None
    code: str | None = None
    #: The deployment cut from this image, on the last line of a build whose
    #: request named a service to deploy.
    release: str | None = None
    next: str | None = None


@dataclass
class HealthCheck:
    type: str | None = None
    path: str | None = None
    test: list[str] | None = None
    interval: int | None = None
    timeout: int | None = None
    grace: int | None = None
    healthy_threshold: int | None = None


@dataclass
class Service:
    org_id: str | None = None
    id: str = ""
    name: str = ""
    app: str | None = None
    depends_on: list[str] | None = None
    replicas: int = 0
    knobs: Knobs = field(default_factory=Knobs)
    health: HealthCheck | None = None
    url: str | None = None
    custom_domain: str | None = None
    release_id: str | None = None
    volume_id: str | None = None
    repo: str | None = None
    branch: str | None = None
    autodeploy: bool = False
    created_at: int = 0
    labels: dict[str, str] | None = None
    url_auth: str | None = None


@dataclass
class RepoRef:
    """A repository the fleet's GitHub App is installed on, at a ref."""

    repo: str = ""
    ref: str = ""


@dataclass
class CreateServiceRequest:
    name: str = ""
    app: str | None = None
    release: str | None = None
    build: str | None = None
    replicas: int | None = None
    knobs: KnobsPatch | None = None
    health: HealthCheck | None = None
    domain: str | None = None
    private: bool | None = None
    custom_domain: str | None = None
    volume: str | None = None
    env: dict[str, str] | None = None
    secret_env: dict[str, str] | None = None
    labels: dict[str, str] | None = None
    url_auth: str | None = None
    repo: str | None = None
    branch: str | None = None
    autodeploy: bool | None = None


@dataclass
class DeployRequest:
    release: str | None = None
    build: str | None = None
    knobs: KnobsPatch | None = None


@dataclass
class PromoteRequest:
    custom_domain: str | None = None
    replicas: int | None = None
    health: HealthCheck | None = None


@dataclass
class RedeployRequest:
    image: str = ""
    release: str | None = None


@dataclass
class ResizeMachineRequest:
    """Boots a machine again at a NEW SIZE, in place: same id, same URL, same
    disk, same volume.

    A boot rather than a resume, because a memory image cannot be loaded into a
    differently-sized VM, so whatever was in memory is lost. Zero on a dimension
    leaves that dimension alone, which is how "give it more memory" is said
    without restating the vCPU count.
    """

    vcpus: int = 0
    mem_mib: int = 0


@dataclass
class Release:
    id: str = ""
    service_id: str = ""
    rootfs_build_id: str | None = None
    mem_build_id: str | None = None
    healthy: bool = False
    created_at: int = 0


@dataclass
class Volume:
    org_id: str | None = None
    id: str = ""
    name: str = ""
    size_gib: int = 0
    machine_id: str | None = None
    host_id: str | None = None
    mount_path: str = ""
    created_at: int = 0


@dataclass
class MachineVolume:
    volume_id: str = ""
    mount_path: str = ""
    device: str = ""
    cache_type: str = ""


@dataclass
class CreateVolumeRequest:
    name: str = ""
    size_gib: int = 0
    mount_path: str | None = None


@dataclass
class Host:
    id: str = ""
    public_ip: str | None = None
    wg_addr: str | None = None
    cpu_free: int = 0
    mem_free_mib: int = 0
    last_seen: int = 0
    alive: bool = False
    cpu_vendor: str | None = None


@dataclass
class WhoamiResponse:
    org_id: str = ""
    scopes: list[str] = field(default_factory=list)
    host_id: str = ""


@dataclass
class AddDomainRequest:
    service_id: str = ""
    hostname: str = ""


@dataclass
class DomainResponse:
    hostname: str = ""
    service_id: str = ""
    verified: bool = False
    cname_target: str = ""
    created_at: int = 0


@dataclass
class HealthResponse:
    ok: bool = False
    host_id: str = ""
    reflink: bool = False
    hugepages: bool = False
    store_version: int = 0
    cpu_vendor: str = ""
    cpu_vendor_forced: bool | None = None
    store_versions: dict[str, int] | None = None
    replication_complete: bool = False


@dataclass
class ErrorResponse:
    error: str = ""
    code: str | None = None
    next: str | None = None
    details: Any = None


@dataclass
class HealthGateDetails:
    service: str = ""
    replica: str = ""
    release: str = ""
    grace_sec: int = 0
    last: HealthLast = field(default_factory=lambda: HealthLast())


@dataclass
class HealthLast:
    status: int | None = None
    body: str | None = None
    error: str | None = None


@dataclass
class UpdateServiceRequest:
    replicas: int | None = None
    health: HealthCheck | None = None
    env: dict[str, str] | None = None
    secret_env: dict[str, str] | None = None
    repo: str | None = None
    branch: str | None = None
    autodeploy: bool | None = None
    domain: str | None = None
    url_auth: str | None = None


@dataclass
class CreateAPIKeyRequest:
    org_id: str = ""
    scopes: list[str] = field(default_factory=list)
    #: What every machine and service this key names must start with. None
    #: means no naming restriction.
    name_prefix: str | None = None
    #: How many machines carrying that prefix may exist at once. None means no cap.
    max_machines: int | None = None
    #: Unix seconds after which the key authenticates nothing. None means it
    #: lives until it is revoked. Write-once with the key.
    expires_at: int | None = None


@dataclass
class APIKeyResponse:
    #: The key itself, returned once, by the call that minted it.
    key: str | None = None
    hash: str = ""
    org_id: str = ""
    scopes: list[str] = field(default_factory=list)
    created_at: int = 0
    revoked_at: int | None = None
    #: The restrictions this key carries. Absent means unrestricted.
    name_prefix: str | None = None
    max_machines: int | None = None
    expires_at: int | None = None


@dataclass
class RevokeResponse:
    hash: str = ""
    revoked_at: int = 0


@dataclass
class ConnectRepoRequest:
    repo: str = ""


@dataclass
class RepoLinkResponse:
    repo: str = ""
    org_id: str = ""
    connected_at: int = 0


@dataclass
class RepoLinkListResponse:
    repos: list[RepoLinkResponse] = field(default_factory=list)


@dataclass
class QuotaResponse:
    org_id: str = ""
    max_machines: int = 0
    max_vcpus: int = 0
    max_mem_mib: int = 0
    max_volume_gib: int = 0
    max_builds: int = 0
    max_snapshot_gib: int = 0
    updated_at: int = 0


@dataclass
class QuotaExceededResponse:
    error: str = ""
    code: str = ""
    next: str = ""
    quota: str = ""
    limit: int = 0
    used: int = 0
    scope: str | None = None


@dataclass
class UsageTotals:
    machine_seconds: int = 0
    vcpu_seconds: int = 0
    mib_seconds: int = 0
    volume_gib_seconds: int = 0
    snapshot_gib_seconds: int = 0


@dataclass
class UsageResponse:
    host_id: str = ""
    since: int = 0
    until: int = 0
    orgs: dict[str, UsageTotals] = field(default_factory=dict)
    machines: dict[str, dict[str, UsageTotals]] | None = None


# ---------------------------------------------------------------------------
# ``internal/compose``, mirrored under a Compose prefix.
# ---------------------------------------------------------------------------


@dataclass
class ComposeRequest:
    compose: str = ""
    env: dict[str, str] | None = None


@dataclass
class ComposeBuild:
    context: str | None = None
    dockerfile: str | None = None


@dataclass
class ComposeVolume:
    name: str = ""
    size_gib: int = 0
    mount_path: str = ""


@dataclass
class ComposeStep:
    name: str = ""
    build: ComposeBuild | None = None
    dockerfile: str | None = None
    dockerfile_append: str | None = None
    env: dict[str, str] | None = None
    secret_refs: dict[str, str] | None = None
    ports: list[int] | None = None
    health: HealthCheck | None = None
    volumes: list[ComposeVolume] | None = None
    replicas: int = 0
    vcpus: int = 0
    mem_mib: int = 0
    depends_on: list[str] | None = None
    knobs: KnobsPatch | None = None
    domain: str | None = None
    private: bool | None = None
    custom_domain: str | None = None
    pre_deploy: str | None = None
    processes: list["ComposeProcess"] | None = None


@dataclass
class ComposeProcess:
    """One named command inside a machine that runs several.

    Filled when several compose services share one build context and therefore
    run as one machine. The ordinary case is one service, one machine, one
    process named ``app``, and this is then absent.
    """

    name: str = ""
    cmd: str | None = None
    needs: list[str] | None = None
    port: bool | None = None


@dataclass
class ComposePlan:
    app: str = ""
    steps: list[ComposeStep] = field(default_factory=list)


@dataclass
class ComposeUnsupported:
    service: str = ""
    key: str = ""
    message: str = ""


@dataclass
class ComposePlanError:
    """The 400 body of a compose plan the planner will not accept. The
    exception a caller catches is ``pilots.ComposePlanError``; this is its
    wire shape."""

    error: str = ""
    code: str = ""
    next: str = ""
    unsupported: list[ComposeUnsupported] = field(default_factory=list)


@dataclass
class ComposePlanResponse:
    plan: ComposePlan = field(default_factory=ComposePlan)
    detected: list[ComposeDetected] = field(default_factory=list)


@dataclass
class ComposeDetected:
    service: str = ""
    source: str = ""
    framework: str | None = None
    dir: str = ""
    port: int = 0
    health: HealthCheck | None = None
    notes: list[str] | None = None


@dataclass
class ComposeUnknownDetails:
    dir: str = ""
    looked_for: list[str] = field(default_factory=list)
    listing: list[str] = field(default_factory=list)
    manifests: dict[str, str] | None = None
    workspaces: list[str] | None = None
    rules: list[str] = field(default_factory=list)


# ---------------------------------------------------------------------------
# Exec stream frame prefixes. Byte-compatible with the sprites protocol;
# mirrors the FrameXxx constants in internal/api/types.go.
# ---------------------------------------------------------------------------

FrameStdin = 0
FrameStdout = 1
FrameStderr = 2
FrameExit = 3
FrameStdinEOF = 4


# ---------------------------------------------------------------------------
# Encoding and decoding.
# ---------------------------------------------------------------------------


def to_json(value: Any) -> Any:
    """A dataclass (or dict, list) as the JSON hostd accepts: ``None`` fields
    are dropped, which is what ``omitempty`` means on the other side."""
    if dataclasses.is_dataclass(value) and not isinstance(value, type):
        out: dict[str, Any] = {}
        for f in dataclasses.fields(value):
            v = getattr(value, f.name)
            if v is None:
                continue
            out[f.name] = to_json(v)
        return out
    if isinstance(value, dict):
        return {k: to_json(v) for k, v in value.items() if v is not None}
    if isinstance(value, list):
        return [to_json(v) for v in value]
    return value


def _unwrap_optional(hint: Any) -> Any:
    origin = get_origin(hint)
    if origin is typing.Union or (origin is not None and str(origin) == "<class 'types.UnionType'>"):
        args = [a for a in get_args(hint) if a is not type(None)]
        return args[0] if len(args) == 1 else Any
    return hint


def _convert(hint: Any, raw: Any) -> Any:
    if raw is None:
        return None
    hint = _unwrap_optional(hint)
    if dataclasses.is_dataclass(hint) and isinstance(hint, type):
        return from_json(hint, raw) if isinstance(raw, dict) else raw
    origin = get_origin(hint)
    if origin in (list,) and isinstance(raw, list):
        (inner,) = get_args(hint) or (Any,)
        return [_convert(inner, r) for r in raw]
    if origin in (dict,) and isinstance(raw, dict):
        args = get_args(hint)
        inner = args[1] if len(args) == 2 else Any
        return {k: _convert(inner, v) for k, v in raw.items()}
    return raw


def from_json(cls: type[T], data: dict[str, Any]) -> T:
    """Builds a dataclass from a decoded body, recursing into nested
    dataclasses and lists of them. Keys the class does not know are ignored:
    a newer host may say more than this version was written for."""
    hints = get_type_hints(cls)
    kwargs: dict[str, Any] = {}
    for f in dataclasses.fields(cls):  # type: ignore[arg-type]
        if f.name in data:
            kwargs[f.name] = _convert(hints.get(f.name, Any), data[f.name])
    return cls(**kwargs)
