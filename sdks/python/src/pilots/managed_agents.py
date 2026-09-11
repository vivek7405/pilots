"""``pilots.managed_agents`` -- Claude Managed Agents on your own machines.

    pip install 'pilots-sdk[anthropic]'

    import asyncio, os
    from pilots.managed_agents import run_worker

    asyncio.run(run_worker(
        environment_id=os.environ["ANTHROPIC_ENVIRONMENT_ID"],
        environment_key=os.environ["ANTHROPIC_ENVIRONMENT_KEY"],
    ))

Anthropic runs the agent loop and the model; every tool call executes inside a
pilots machine you own. The worker polls Anthropic for work items, and for
each one creates a machine, runs Anthropic's tool runner inside it with that
session's ``ANTHROPIC_*`` environment, and destroys the machine when the
session ends. The agent's filesystem, its processes and its network egress
never leave your fleet.

Two shapes, chosen by ``machine_name``:

* omitted -- one machine per session, named ``agent-<session>``, destroyed
  when the session ends. The isolation an untrusted run wants.
* given -- one long-lived machine every session runs in, never destroyed. The
  environment survives between sessions, which is what a personal agent wants.

The runner inside the machine is ``ant beta:worker run`` by default, which
needs Node on the image; the pilots guest image has Node 24 on ``PATH``.
``runner_command`` replaces it with the Python SDK worker, or with anything
else that speaks the same environment.
"""

from __future__ import annotations

import asyncio
import logging
import shlex
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from typing import Any

from .client import PilotsClient
from .errors import NotFoundError, PilotsError
from .types import Machine

logger = logging.getLogger("pilots.managed_agents")

#: What runs inside the machine, per work item. It reads the ANTHROPIC_*
#: variables the worker passes and handles exactly one session.
DEFAULT_RUNNER = "npx -y @anthropic-ai/ant beta:worker run --workdir /workspace"

#: The variables Anthropic's tool runner expects to find.
SANDBOX_ENV = (
    "ANTHROPIC_ENVIRONMENT_ID",
    "ANTHROPIC_ENVIRONMENT_KEY",
    "ANTHROPIC_WORK_ID",
    "ANTHROPIC_SESSION_ID",
    "ANTHROPIC_WORK_SECRET",
    "ANTHROPIC_BASE_URL",
)


@dataclass
class WorkItem:
    """One unit of work claimed from Anthropic: a session to run."""

    work_id: str
    session_id: str
    secret: str = ""

    def env(self, environment_id: str, environment_key: str, base_url: str | None = None) -> dict[str, str]:
        out = {
            "ANTHROPIC_ENVIRONMENT_ID": environment_id,
            "ANTHROPIC_ENVIRONMENT_KEY": environment_key,
            "ANTHROPIC_WORK_ID": self.work_id,
            "ANTHROPIC_SESSION_ID": self.session_id,
            "ANTHROPIC_WORK_SECRET": self.secret,
        }
        if base_url:
            out["ANTHROPIC_BASE_URL"] = base_url
        return out


@dataclass
class WorkerConfig:
    environment_id: str
    environment_key: str
    #: Reuse one machine for every session instead of one per session.
    machine_name: str | None = None
    #: What runs inside the machine per work item.
    runner_command: str = DEFAULT_RUNNER
    #: The working directory the runner is given; created if absent.
    workdir: str = "/workspace"
    #: Extra environment for every session (a proxy, a registry token).
    env: dict[str, str] = field(default_factory=dict)
    base_url: str | None = None
    #: Seconds a single session may run before its command is killed.
    session_timeout: float = 3600.0
    #: Checkpoint the machine before each session, so a run can be rewound.
    checkpoint_before_session: bool = False


class MachineRunner:
    """Runs one work item inside a pilots machine.

    Importable and testable without the Anthropic SDK: ``run_item`` takes a
    ``WorkItem`` and does the machine half. ``run_worker`` below is what wires
    it to Anthropic's poller.
    """

    def __init__(self, config: WorkerConfig, *, api_key: str | None = None, base_url: str | None = None,
                 client: PilotsClient | None = None) -> None:
        self.config = config
        self._client = client or PilotsClient(api_key, base_url=base_url, timeout=600.0)

    def _machine_name(self, item: WorkItem) -> str:
        if self.config.machine_name:
            return self.config.machine_name
        # A session id is long and may carry characters a label cannot; the
        # tail is enough to tell two live sessions apart.
        return f"agent-{item.session_id[-12:].lower().replace('_', '-')}"

    def _find(self, name: str) -> Machine | None:
        for m in self._client.machines.list():
            if m.name == name:
                return m
        return None

    def ensure_machine(self, item: WorkItem) -> Machine:
        name = self._machine_name(item)
        machine = self._find(name)
        if machine is None:
            machine = self._client.machines.create(name=name, labels={"anthropic-session": item.session_id})
            logger.info("created machine %s for session %s", machine.name, item.session_id)
        return machine

    def run_item(self, item: WorkItem) -> dict[str, Any]:
        """Create or find the machine, run the tool runner in it, and clean up.

        Returns a summary dict rather than raising, so one failed session does
        not stop the worker: a run that could not start is a result the caller
        logs and moves past.
        """
        machine: Machine | None = None
        owned = self.config.machine_name is None
        try:
            machine = self.ensure_machine(item)
            if self.config.checkpoint_before_session:
                ck = self._client.machines.checkpoint(machine.id, f"before {item.session_id}")
                logger.info("checkpointed %s as %s", machine.name, ck.id)
            session_env = item.env(
                self.config.environment_id, self.config.environment_key, self.config.base_url
            )
            env = {**self.config.env, **session_env}
            self._client.machines.exec(machine.id, cmd=f"mkdir -p -- {shlex.quote(self.config.workdir)}")
            res = self._client.machines.exec(
                machine.id,
                cmd=self.config.runner_command,
                cwd=self.config.workdir,
                env=env,
                timeout_ms=int(self.config.session_timeout * 1000),
            )
            logger.info("session %s finished with exit code %s", item.session_id, res.exit_code)
            return {
                "session_id": item.session_id,
                "work_id": item.work_id,
                "machine": machine.name,
                "url": machine.url,
                "exit_code": res.exit_code,
                "stdout": res.stdout,
                "stderr": res.stderr,
            }
        except PilotsError as err:
            logger.error("session %s failed: %s", item.session_id, err)
            return {"session_id": item.session_id, "work_id": item.work_id, "error": str(err),
                    "code": err.code, "next": err.next}
        finally:
            if owned and machine is not None:
                try:
                    self._client.machines.destroy(machine.id)
                    logger.info("destroyed machine %s", machine.name)
                except NotFoundError:
                    pass
                except PilotsError as err:
                    logger.warning("could not destroy %s: %s", machine.name, err)

    async def run_item_async(self, item: WorkItem) -> dict[str, Any]:
        return await asyncio.to_thread(self.run_item, item)

    def close(self) -> None:
        self._client.close()


async def run_worker(
    *,
    environment_id: str,
    environment_key: str,
    machine_name: str | None = None,
    runner_command: str = DEFAULT_RUNNER,
    workdir: str = "/workspace",
    env: dict[str, str] | None = None,
    base_url: str | None = None,
    session_timeout: float = 3600.0,
    checkpoint_before_session: bool = False,
    api_key: str | None = None,
    pilots_base_url: str | None = None,
    drain: bool = False,
    on_result: Callable[[dict[str, Any]], Awaitable[None] | None] | None = None,
) -> None:
    """Poll Anthropic for work and run each session inside a pilots machine.

    Needs the ``anthropic`` extra. Runs until cancelled, or until the queue is
    empty when ``drain=True``.
    """
    from anthropic import AsyncAnthropic

    config = WorkerConfig(
        environment_id=environment_id,
        environment_key=environment_key,
        machine_name=machine_name,
        runner_command=runner_command,
        workdir=workdir,
        env=env or {},
        base_url=base_url,
        session_timeout=session_timeout,
        checkpoint_before_session=checkpoint_before_session,
    )
    runner = MachineRunner(config, api_key=api_key, base_url=pilots_base_url)
    try:
        async with AsyncAnthropic(auth_token=environment_key) as anthropic:
            poller = anthropic.beta.environments.work.poller(
                environment_id=environment_id,
                environment_key=environment_key,
                block_ms=None,
                reclaim_older_than_ms=2000,
                drain=drain,
                auto_stop=False,
            )
            async for work in poller:
                item = WorkItem(work_id=work.id, session_id=work.data.id, secret=getattr(work, "secret", "") or "")
                result = await runner.run_item_async(item)
                if on_result is not None:
                    outcome = on_result(result)
                    if asyncio.iscoroutine(outcome):
                        await outcome
    finally:
        runner.close()


__all__ = ["run_worker", "MachineRunner", "WorkerConfig", "WorkItem", "DEFAULT_RUNNER", "SANDBOX_ENV"]
