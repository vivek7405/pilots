"""``pilots.openai_agents`` -- a pilots-backed sandbox for the OpenAI Agents SDK.

    pip install 'pilots-sdk[openai-agents]'

    from agents.run import RunConfig
    from agents.sandbox import SandboxAgent, SandboxRunConfig
    from pilots.openai_agents import PilotsSandboxClient

    client = PilotsSandboxClient()
    sandbox = await client.create()
    agent = SandboxAgent(name="pilots assistant", instructions="Do the task in the workspace.")
    run_config = RunConfig(sandbox=SandboxRunConfig(session=sandbox))

By default the client creates an ephemeral machine and destroys it when the
session is cleaned up. ``PilotsSandboxClientOptions(machine_name="…")``
attaches to an existing one and leaves it alone.

The provider contract moves quickly, so this adapter deliberately keeps its
own surface small and synchronous underneath: everything runs through
``pilots.PilotsClient`` on a worker thread. ``PilotsSandboxSession`` is usable
on its own -- ``exec``, ``read``, ``write``, ``snapshot``, ``restore`` -- which
is what the tests drive, and what an integration that is not the Agents SDK
can hold.
"""

from __future__ import annotations

import asyncio
import base64
import uuid
from dataclasses import dataclass, field
from typing import Any

from .client import PilotsClient
from .errors import NotFoundError, PilotsError
from .types import Machine

#: Where a session's files live, and what a harness means by /workspace.
DEFAULT_WORKDIR = "/home/pilot/workspace"


def _quote(s: str) -> str:
    return "'" + s.replace("'", "'\\''") + "'"


@dataclass
class PilotsSandboxClientOptions:
    """Options for one ``create()``."""

    #: Attach to this machine instead of creating one. It is never destroyed.
    machine_name: str | None = None
    #: The directory ``/workspace`` maps to.
    workdir: str = DEFAULT_WORKDIR
    #: Environment for every command in the session.
    env: dict[str, str] = field(default_factory=dict)
    #: vCPUs and memory for a machine this client creates.
    vcpus: int | None = None
    mem_mib: int | None = None


@dataclass
class ExecResult:
    stdout: str
    stderr: str
    exit_code: int


class PilotsSandboxSession:
    """One machine, for the duration of one agent run."""

    provider = "pilots"

    def __init__(self, client: PilotsClient, machine: Machine, *, workdir: str, env: dict[str, str],
                 owned: bool) -> None:
        self._client = client
        self._machine = machine
        self.workdir = workdir
        self.env = dict(env)
        #: True when this session created the machine and may destroy it.
        self.owned = owned

    # -- identity ---------------------------------------------------------------

    @property
    def id(self) -> str:
        return self._machine.name

    @property
    def machine_id(self) -> str:
        return self._machine.id

    @property
    def url(self) -> str:
        return self._machine.url

    @property
    def workspace_root(self) -> str:
        """The real path behind the virtual ``/workspace``. A harness CLI
        interprets cwd literally, so it needs this rather than the virtual one."""
        return self.workdir

    def _abs(self, path: str) -> str:
        p = str(path)
        if p == "/workspace":
            return self.workdir
        if p.startswith("/workspace/"):
            return self.workdir.rstrip("/") + p[len("/workspace") :]
        if p.startswith("/"):
            return p
        return f"{self.workdir.rstrip('/')}/{p}"

    # -- process -----------------------------------------------------------------

    def exec_sync(self, command: str | list[str], *, cwd: str | None = None,
                  env: dict[str, str] | None = None, timeout: float | None = None) -> ExecResult:
        cmd = command if isinstance(command, str) else " ".join(_quote(a) for a in command)
        merged = {**self.env, **(env or {})}
        res = self._client.machines.exec(
            self._machine.id,
            cmd=cmd,
            cwd=self._abs(cwd) if cwd else self.workdir,
            env=merged or None,
            timeout_ms=int(timeout * 1000) if timeout else None,
        )
        return ExecResult(res.stdout, res.stderr, res.exit_code)

    async def exec(self, command: str | list[str], **kw: Any) -> ExecResult:
        return await asyncio.to_thread(lambda: self.exec_sync(command, **kw))

    def spawn(self, command: str | list[str], **kw: Any) -> Any:
        """A long-running process: the raw exec stream, for a caller that
        wants to read it as it goes."""
        argv = command if isinstance(command, list) else ["sh", "-c", command]
        return self._client.machines.exec_stream(
            self._machine.id, argv, cwd=kw.get("cwd", self.workdir), env={**self.env, **(kw.get("env") or {})}
        )

    # -- filesystem ---------------------------------------------------------------

    def read_sync(self, path: str) -> bytes:
        res = self.exec_sync(f"base64 -w0 -- {_quote(self._abs(path))}")
        if res.exit_code != 0:
            raise PilotsError(res.stderr.strip() or f"cannot read {path}")
        return base64.b64decode(res.stdout)

    async def read(self, path: str) -> bytes:
        return await asyncio.to_thread(self.read_sync, path)

    def write_sync(self, path: str, data: bytes | str) -> None:
        raw = data.encode() if isinstance(data, str) else data
        target = self._abs(path)
        encoded = base64.b64encode(raw).decode()
        res = self.exec_sync(
            f"mkdir -p -- $(dirname {_quote(target)}) && printf %s {_quote(encoded)} | base64 -d > {_quote(target)}"
        )
        if res.exit_code != 0:
            raise PilotsError(res.stderr.strip() or f"cannot write {path}")

    async def write(self, path: str, data: bytes | str) -> None:
        await asyncio.to_thread(self.write_sync, path, data)

    def listdir_sync(self, path: str = "/workspace") -> list[str]:
        res = self.exec_sync(f"ls -1A -- {_quote(self._abs(path))}")
        if res.exit_code != 0:
            raise PilotsError(res.stderr.strip() or f"cannot list {path}")
        return [line for line in res.stdout.splitlines() if line]

    # -- snapshots -----------------------------------------------------------------

    def snapshot_sync(self, label: str | None = None) -> str:
        return self._client.machines.checkpoint(self._machine.id, label).id

    async def snapshot(self, label: str | None = None) -> str:
        """A checkpoint id. Restoring it puts the whole machine back."""
        return await asyncio.to_thread(self.snapshot_sync, label)

    def restore_sync(self, checkpoint_id: str) -> None:
        self._machine = self._client.checkpoints.restore(checkpoint_id)

    async def restore(self, checkpoint_id: str) -> None:
        """IN PLACE: the same machine, the same URL. Destructive: everything
        after the checkpoint is discarded."""
        await asyncio.to_thread(self.restore_sync, checkpoint_id)

    # -- ports -------------------------------------------------------------------

    async def expose_port(self, port: int) -> str:
        """The machine's URL. A pilots machine serves one HTTP port on its
        permanent address, so exposing a port is reading that address."""
        return self._machine.url

    # -- lifecycle -----------------------------------------------------------------

    @property
    def capabilities(self) -> dict[str, bool]:
        return {
            "fs": True,
            "exec": True,
            "env": True,
            "ports": True,
            "background_processes": True,
            "writable_stdin": True,
            "killable_processes": True,
            "snapshots": True,
            "network_policy": False,
            "durable_filesystem": True,
            "fork": False,
        }

    def close_sync(self) -> None:
        if self.owned:
            try:
                self._client.machines.destroy(self._machine.id)
            except NotFoundError:
                pass

    async def close(self) -> None:
        await asyncio.to_thread(self.close_sync)

    shutdown = close

    async def __aenter__(self) -> PilotsSandboxSession:
        return self

    async def __aexit__(self, *exc: object) -> None:
        await self.close()


class PilotsSandboxClient:
    """Creates and resumes sessions."""

    def __init__(self, api_key: str | None = None, *, base_url: str | None = None,
                 timeout: float = 600.0, client: PilotsClient | None = None) -> None:
        self._client = client or PilotsClient(api_key, base_url=base_url, timeout=timeout)

    def _find(self, name: str) -> Machine | None:
        for m in self._client.machines.list():
            if m.name == name:
                return m
        return None

    def create_sync(self, options: PilotsSandboxClientOptions | None = None) -> PilotsSandboxSession:
        opts = options or PilotsSandboxClientOptions()
        owned = opts.machine_name is None
        name = opts.machine_name or f"agents-{uuid.uuid4().hex[:8]}"
        machine = self._find(name)
        if machine is None:
            body: dict[str, Any] = {"name": name}
            if opts.vcpus:
                body["vcpus"] = opts.vcpus
            if opts.mem_mib:
                body["mem_mib"] = opts.mem_mib
            machine = self._client.machines.create(body)
        session = PilotsSandboxSession(self._client, machine, workdir=opts.workdir, env=opts.env, owned=owned)
        session.exec_sync(f"mkdir -p -- {_quote(opts.workdir)}")
        return session

    async def create(self, options: PilotsSandboxClientOptions | None = None) -> PilotsSandboxSession:
        return await asyncio.to_thread(self.create_sync, options)

    async def resume(
        self, session_id: str, options: PilotsSandboxClientOptions | None = None
    ) -> PilotsSandboxSession | None:
        """Reattach to a machine by name. None when it is gone."""
        opts = options or PilotsSandboxClientOptions()

        def _resume() -> PilotsSandboxSession | None:
            machine = self._find(session_id)
            if machine is None:
                return None
            return PilotsSandboxSession(self._client, machine, workdir=opts.workdir, env=opts.env, owned=False)

        return await asyncio.to_thread(_resume)

    async def delete(self, session: PilotsSandboxSession) -> None:
        await asyncio.to_thread(lambda: self._client.machines.destroy(session.machine_id))

    def close(self) -> None:
        self._client.close()


__all__ = [
    "PilotsSandboxClient",
    "PilotsSandboxClientOptions",
    "PilotsSandboxSession",
    "ExecResult",
    "DEFAULT_WORKDIR",
]
