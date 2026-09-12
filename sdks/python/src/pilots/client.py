"""``PilotsClient``: one method per route, grouped by the noun the route acts
on. Every host serves this identical API, so the base URL is any host in the
fleet (or the fleet's API name). Nothing here is aware of which host owns a
machine: hostd forwards a write that arrived at the wrong host itself.
"""

from __future__ import annotations

import builtins
import os
from collections.abc import Iterator
from typing import Any
from urllib.parse import quote

import httpx

from ._http import DEFAULT_TIMEOUT, Http, text_lines
from .builds import BuildStream
from .errors import PilotsError
from .stream import ExecStream, build_exec_url
from .types import (
    AddDomainRequest,
    APIKeyResponse,
    Checkpoint,
    CheckpointRequest,
    ComposePlan,
    ComposePlanResponse,
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
    RepoLinkListResponse,
    RepoLinkResponse,
    RepoRef,
    ResizeMachineRequest,
    RevokeResponse,
    Service,
    UpdateMachineRequest,
    UpdateServiceRequest,
    UsageResponse,
    Volume,
    WhoamiResponse,
    from_json,
    to_json,
)

Tar = bytes | Iterator[bytes]


def _seg(s: str) -> str:
    return quote(s, safe="")


class PilotsClient:
    """The client.

    ``api_key`` falls back to ``PILOT_API_KEY``; ``base_url`` to
    ``PILOT_API_URL``, then ``https://api.pilotrun.app``. ``timeout`` (seconds)
    applies to JSON calls only: builds, log follows, deploys and streams get
    no deadline. ``org`` makes an ADMIN key act as one org. ``http_client`` is
    the seam for retries, pooling or tracing.
    """

    def __init__(
        self,
        api_key: str | None = None,
        *,
        base_url: str | None = None,
        timeout: float = DEFAULT_TIMEOUT,
        org: str | None = None,
        http_client: httpx.Client | None = None,
    ) -> None:
        key = api_key or os.environ.get("PILOT_API_KEY", "")
        self.http = Http(key, base_url=base_url, timeout=timeout, org=org, client=http_client)
        self.machines = Machines(self.http)
        self.builds = Builds(self.http)
        self.checkpoints = Checkpoints(self.http)
        self.services = Services(self.http)
        self.domains = Domains(self.http)
        self.volumes = Volumes(self.http)
        self.hosts = Hosts(self.http)
        self.api_keys = APIKeys(self.http)
        self.repos = Repos(self.http)
        self.quotas = Quotas(self.http)
        self.usage = Usage(self.http)
        self.compose = Compose(self.http)

    @property
    def base_url(self) -> str:
        return self.http.base_url

    @property
    def api_key(self) -> str:
        return self.http.api_key

    def close(self) -> None:
        self.http.close()

    def __enter__(self) -> PilotsClient:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()

    def health(self) -> HealthResponse:
        """Liveness. The one route that needs no key."""
        return from_json(HealthResponse, self.http.json("GET", "/v1/health"))

    def whoami(self) -> WhoamiResponse:
        """The org, scopes and host this client's key resolves to."""
        return from_json(WhoamiResponse, self.http.json("GET", "/v1/whoami"))

    def plan(self, tar: Tar, *, app: str | None = None) -> ComposePlanResponse:
        """Asks the host what a directory is, from a tar of it. The front
        door: what a caller reaches for before it knows whether the directory
        is a compose project, a Dockerfile or a framework the platform
        recognises. No client deadline: the upload is a whole source tree."""
        res = self.http.send(
            "POST", "/v1/plan", content=tar, content_type="application/x-tar", query={"app": app}, timeout=None
        )
        return from_json(ComposePlanResponse, res.json())

    def plan_repo(self, ref: RepoRef | dict[str, str], *, app: str | None = None) -> ComposePlanResponse:
        """Asks the host what a REPOSITORY is, naming it rather than sending
        it; the host fetches the ref through the fleet's GitHub App."""
        res = self.http.send("POST", "/v1/plan", json_body=to_json(ref), query={"app": app}, timeout=None)
        return from_json(ComposePlanResponse, res.json())


class Machines:
    def __init__(self, http: Http) -> None:
        self._http = http

    def create(self, req: CreateMachineRequest | dict[str, Any] | None = None, **fields: Any) -> Machine:
        """No client deadline: a create from a build is a kernel boot, and an
        abort leaves a machine running that the caller has no id for."""
        body = to_json(req) if req is not None else {}
        body.update({k: v for k, v in fields.items() if v is not None})
        return from_json(Machine, self._http.json("POST", "/v1/machines", json_body=body, timeout=None))

    def list(self) -> builtins.list[Machine]:
        return [from_json(Machine, m) for m in self._http.json("GET", "/v1/machines") or []]

    def get(self, id: str) -> Machine:
        return from_json(Machine, self._http.json("GET", f"/v1/machines/{_seg(id)}"))

    def update(self, id: str, req: UpdateMachineRequest | dict[str, Any]) -> Machine:
        return from_json(Machine, self._http.json("PATCH", f"/v1/machines/{_seg(id)}", json_body=to_json(req)))

    def destroy(self, id: str) -> None:
        self._http.none("DELETE", f"/v1/machines/{_seg(id)}")

    def exec(self, id: str, req: ExecRequest | dict[str, Any] | None = None, **fields: Any) -> ExecResponse:
        """Buffered exec. A ``timeout_ms`` longer than the client's own
        deadline extends it, with a margin, so the server's own timeout result
        arrives rather than a client abort."""
        body = to_json(req) if req is not None else {}
        body.update({k: v for k, v in fields.items() if v is not None})
        timeout: Any = DEFAULT_TIMEOUT
        if body.get("timeout_ms"):
            extended = body["timeout_ms"] / 1000 + 5
            if extended > self._http.timeout:
                timeout = extended
        return from_json(
            ExecResponse, self._http.json("POST", f"/v1/machines/{_seg(id)}/exec", json_body=body, timeout=timeout)
        )

    def exec_stream(
        self,
        id: str,
        argv: builtins.list[str],
        *,
        cwd: str | None = None,
        env: dict[str, str] | None = None,
        user: str | None = None,
        stdin: bool = False,
        tty: bool = False,
        rows: int | None = None,
        cols: int | None = None,
    ) -> ExecStream:
        """Streams a command's output frame by frame. ``stdin`` is False by
        default; ``tty=True`` runs it on a pseudo-terminal, implies stdin and
        merges stderr into stdout."""
        url = build_exec_url(
            self._http.base_url,
            f"/v1/machines/{_seg(id)}/exec/stream",
            argv,
            cwd=cwd,
            env=env,
            user=user,
            stdin=stdin,
            tty=tty,
            rows=rows,
            cols=cols,
            org=self._http.org,
        )
        return ExecStream(url, self._http.api_key, stdin=stdin or tty, tty=tty)

    def logs(self, id: str) -> str:
        return self._http.text("GET", f"/v1/machines/{_seg(id)}/logs")

    def follow_logs(self, id: str) -> Iterator[str]:
        """Follows the console log line by line. Never given a client deadline."""
        res = self._http.send(
            "GET", f"/v1/machines/{_seg(id)}/logs", query={"follow": "1"}, timeout=None, stream=True
        )
        return text_lines(res)

    def suspend(self, id: str) -> None:
        self._http.none("POST", f"/v1/machines/{_seg(id)}/suspend")

    def wake(self, id: str) -> None:
        self._http.none("POST", f"/v1/machines/{_seg(id)}/wake")

    def stop(self, id: str) -> None:
        self._http.none("POST", f"/v1/machines/{_seg(id)}/stop")

    def start(self, id: str) -> None:
        self._http.none("POST", f"/v1/machines/{_seg(id)}/start")

    def resize(self, id: str, vcpus: int = 0, mem_mib: int = 0) -> Machine:
        """Boots a machine again at a new size, in place: same id, same URL, same disk, same volume.

        A boot rather than a resume, because a memory image cannot be loaded into a
        differently-sized VM, so the machine loses what was in memory. Leave a
        dimension at zero to keep it as it is.
        """
        body = to_json(ResizeMachineRequest(vcpus=vcpus, mem_mib=mem_mib))
        return from_json(Machine, self._http.json("POST", f"/v1/machines/{_seg(id)}/resize", json_body=body))

    def checkpoint(self, id: str, comment: str | None = None) -> Checkpoint:
        body = to_json(CheckpointRequest(comment=comment))
        return from_json(Checkpoint, self._http.json("POST", f"/v1/machines/{_seg(id)}/checkpoints", json_body=body))

    def list_checkpoints(self, id: str) -> builtins.list[Checkpoint]:
        return [from_json(Checkpoint, c) for c in self._http.json("GET", f"/v1/machines/{_seg(id)}/checkpoints") or []]

    def promote(self, id: str, req: PromoteRequest | dict[str, Any] | None = None) -> Service:
        """Turns a sandbox into a durable service. The URL does not change."""
        body = to_json(req) if req is not None else {}
        return from_json(Service, self._http.json("POST", f"/v1/machines/{_seg(id)}/promote", json_body=body))

    def volume(self, id: str) -> MachineVolume:
        """The volume drive Firecracker actually has, not the one hostd meant to set."""
        return from_json(MachineVolume, self._http.json("GET", f"/v1/machines/{_seg(id)}/volume"))


class Checkpoints:
    def __init__(self, http: Http) -> None:
        self._http = http

    def restore(self, id: str) -> Machine:
        """Restores IN PLACE: the same machine, keeping its id, URL and agent token."""
        return from_json(Machine, self._http.json("POST", f"/v1/checkpoints/{_seg(id)}/restore"))

    def get(self, id: str) -> Checkpoint:
        """``durable`` flips to True once the upload to object storage lands."""
        return from_json(Checkpoint, self._http.json("GET", f"/v1/checkpoints/{_seg(id)}"))


class Builds:
    def __init__(self, http: Http) -> None:
        self._http = http

    def create(self, tar: Tar, *, deploy: str | None = None) -> BuildStream:
        """Uploads a build context (a tar) and streams the build's NDJSON log.
        ``deploy`` names a service to cut a release for, on the verdict, on
        the HOST, exactly once."""
        res = self._http.send(
            "POST",
            "/v1/builds",
            content=tar,
            content_type="application/x-tar",
            query={"deploy": deploy},
            timeout=None,
            stream=True,
        )
        return BuildStream(res)

    def create_from_repo(
        self, ref: RepoRef | dict[str, str], *, app: str | None = None, deploy: str | None = None
    ) -> BuildStream:
        """Builds a REPOSITORY by naming it; the host fetches the ref through
        the fleet's GitHub App and builds the one step a plan may produce."""
        res = self._http.send(
            "POST", "/v1/builds", json_body=to_json(ref), query={"app": app, "deploy": deploy}, timeout=None, stream=True
        )
        return BuildStream(res)

    def logs(self, id: str, *, follow: bool = False) -> BuildStream:
        """Replays a build's log, following it live when asked."""
        res = self._http.send(
            "GET", f"/v1/builds/{_seg(id)}/logs", query={"follow": "1" if follow else None}, timeout=None, stream=True
        )
        return BuildStream(res, id)


class Services:
    def __init__(self, http: Http) -> None:
        self._http = http

    def create(self, req: CreateServiceRequest | dict[str, Any]) -> Service:
        return from_json(Service, self._http.json("POST", "/v1/services", json_body=to_json(req)))

    def list(self) -> builtins.list[Service]:
        return [from_json(Service, s) for s in self._http.json("GET", "/v1/services") or []]

    def get(self, id: str) -> Service:
        return from_json(Service, self._http.json("GET", f"/v1/services/{_seg(id)}"))

    def deploy(self, id: str, req: DeployRequest | dict[str, Any] | None = None) -> Release:
        """No client deadline: a rollout takes as long as the release takes to
        prove itself, and an abort mid-rollout is the failure mode the server
        side was fixed for."""
        body = to_json(req) if req is not None else {}
        return from_json(Release, self._http.json("POST", f"/v1/services/{_seg(id)}/deploy", json_body=body, timeout=None))

    def rollback(self, id: str) -> Release:
        return from_json(Release, self._http.json("POST", f"/v1/services/{_seg(id)}/rollback", timeout=None))

    def patch(self, id: str, req: UpdateServiceRequest | dict[str, Any]) -> Service:
        """``env``, ``secret_env`` and ``replicas`` REPLACE the stored values
        and take effect at the next deploy. ``knobs`` are refused here and
        travel on ``deploy``."""
        return from_json(Service, self._http.json("PATCH", f"/v1/services/{_seg(id)}", json_body=to_json(req)))

    def releases(self, id: str) -> builtins.list[Release]:
        """Newest first."""
        return [from_json(Release, r) for r in self._http.json("GET", f"/v1/services/{_seg(id)}/releases") or []]


class Domains:
    def __init__(self, http: Http) -> None:
        self._http = http

    def add(self, req: AddDomainRequest | dict[str, Any]) -> DomainResponse:
        return from_json(DomainResponse, self._http.json("POST", "/v1/domains", json_body=to_json(req)))

    def list(self) -> builtins.list[DomainResponse]:
        return [from_json(DomainResponse, d) for d in self._http.json("GET", "/v1/domains") or []]

    def remove(self, hostname: str) -> None:
        self._http.none("DELETE", f"/v1/domains/{_seg(hostname)}")


class Volumes:
    def __init__(self, http: Http) -> None:
        self._http = http

    def create(self, req: CreateVolumeRequest | dict[str, Any]) -> Volume:
        return from_json(Volume, self._http.json("POST", "/v1/volumes", json_body=to_json(req)))

    def list(self) -> builtins.list[Volume]:
        return [from_json(Volume, v) for v in self._http.json("GET", "/v1/volumes") or []]


class Hosts:
    def __init__(self, http: Http) -> None:
        self._http = http

    def list(self) -> builtins.list[Host]:
        """The fleet as this host sees it, read from its local replica."""
        return [from_json(Host, h) for h in self._http.json("GET", "/v1/hosts") or []]


class APIKeys:
    def __init__(self, http: Http) -> None:
        self._http = http

    def create(self, req: CreateAPIKeyRequest | dict[str, Any]) -> APIKeyResponse:
        """The plaintext key is in ``key``, returned by this call and never again."""
        return from_json(APIKeyResponse, self._http.json("POST", "/v1/api-keys", json_body=to_json(req)))

    def revoke(self, hash: str) -> RevokeResponse:
        return from_json(RevokeResponse, self._http.json("POST", f"/v1/api-keys/{_seg(hash)}/revoke"))

    def list(self, org: str) -> builtins.list[APIKeyResponse]:
        return [from_json(APIKeyResponse, k) for k in self._http.json("GET", "/v1/api-keys", query={"org": org}) or []]


class Repos:
    def __init__(self, http: Http) -> None:
        self._http = http

    def connect(self, repo: str) -> RepoLinkResponse:
        """Idempotent: the row is write-once, so connecting twice is one connection."""
        return from_json(RepoLinkResponse, self._http.json("POST", "/v1/repos", json_body={"repo": repo}))

    def list(self) -> builtins.list[RepoLinkResponse]:
        res = from_json(RepoLinkListResponse, self._http.json("GET", "/v1/repos") or {})
        return res.repos


class Quotas:
    def __init__(self, http: Http) -> None:
        self._http = http

    def get(self, org: str) -> QuotaResponse:
        return from_json(QuotaResponse, self._http.json("GET", f"/v1/quotas/{_seg(org)}"))

    def put(self, org: str, quota: QuotaResponse | dict[str, Any]) -> QuotaResponse:
        body = to_json(quota)
        body.pop("updated_at", None)
        return from_json(QuotaResponse, self._http.json("PUT", f"/v1/quotas/{_seg(org)}", json_body=body))


class Usage:
    def __init__(self, http: Http) -> None:
        self._http = http

    def get(self, *, since: int | None = None, until: int | None = None) -> UsageResponse:
        """Unix seconds. Defaults to the last 24 hours on the host that answers."""
        return from_json(UsageResponse, self._http.json("GET", "/v1/usage", query={"since": since, "until": until}))


class Compose:
    def __init__(self, http: Http) -> None:
        self._http = http

    def plan(self, req: ComposeRequest | dict[str, Any]) -> ComposePlan:
        """Plans a compose file into ordered steps. Stateless."""
        return from_json(ComposePlan, self._http.json("POST", "/v1/compose/plan", json_body=to_json(req)))


__all__ = ["PilotsClient", "PilotsError"]
