"""The three framework adapters, against a fake client.

None of these tests import their framework: each adapter's lifecycle half is
plain Python over ``PilotsClient``, and that is the half a wrong change would
break silently. The ADK tool objects themselves need the extra, and the one
test that builds them skips without it.
"""

from __future__ import annotations

import asyncio
import base64
from typing import Any

import pytest

from pilots.adk import MAX_WRITE_BYTES, TOOL_NAMES, MachineSession, PilotsPlugin
from pilots.managed_agents import DEFAULT_RUNNER, MachineRunner, WorkerConfig, WorkItem
from pilots.openai_agents import PilotsSandboxClient, PilotsSandboxClientOptions
from pilots.types import Checkpoint, ExecResponse, Machine


class _Store:
    """A tiny client the adapters can drive, with plain dict storage."""

    def __init__(self) -> None:
        self.machines_by_id: dict[str, Machine] = {}
        self.checkpoint_list: list[Checkpoint] = []
        self.calls: list[tuple[str, str]] = []
        self.exec_env: list[dict[str, str]] = []
        self.exec_handler = lambda kw: ExecResponse(stdout="", stderr="", exit_code=0)
        self.closed = False
        outer = self

        class M:
            def list(self) -> list[Machine]:
                return list(outer.machines_by_id.values())

            def create(self, req: Any = None, **fields: Any) -> Machine:
                body = dict(req or {})
                body.update(fields)
                name = body.get("name", "unnamed")
                m = Machine(id=f"m-{len(outer.machines_by_id) + 1}", name=name, state="running",
                            url=f"https://{name}.pilotrun.app", labels=body.get("labels"))
                outer.machines_by_id[m.id] = m
                outer.calls.append(("create", name))
                return m

            def destroy(self, id: str) -> None:
                outer.machines_by_id.pop(id, None)
                outer.calls.append(("destroy", id))

            def exec(self, id: str, **kw: Any) -> ExecResponse:
                outer.calls.append(("exec", kw.get("cmd", "")))
                outer.exec_env.append(kw.get("env") or {})
                return outer.exec_handler(kw)

            def checkpoint(self, id: str, comment: str | None = None) -> Checkpoint:
                ck = Checkpoint(id=f"ck-{len(outer.checkpoint_list) + 1}", machine_id=id, seq=1,
                                comment=comment or "", created_at=1750000000 + len(outer.checkpoint_list))
                outer.checkpoint_list.append(ck)
                outer.calls.append(("checkpoint", ck.id))
                return ck

            def list_checkpoints(self, id: str) -> list[Checkpoint]:
                return list(outer.checkpoint_list)

        class C:
            def restore(self, id: str) -> Machine:
                outer.calls.append(("restore", id))
                return next(iter(outer.machines_by_id.values()))

        self.machines = M()
        self.checkpoints = C()

    def close(self) -> None:
        self.closed = True


@pytest.fixture
def store() -> _Store:
    return _Store()


# ---------------------------------------------------------------------------
# Google ADK
# ---------------------------------------------------------------------------


def test_adk_creates_lazily_and_reuses_a_named_machine(store: _Store) -> None:
    session = MachineSession(machine_name="my-project", client=store)  # type: ignore[arg-type]
    # Constructing it touches nothing.
    assert store.calls == []
    m1 = session.get_machine()
    m2 = session.get_machine()
    assert m1.id == m2.id
    assert [c for c in store.calls if c[0] == "create"] == [("create", "my-project")]
    # A second session with the same name finds it rather than creating one.
    again = MachineSession(machine_name="my-project", client=store)  # type: ignore[arg-type]
    assert again.get_machine().id == m1.id
    assert len([c for c in store.calls if c[0] == "create"]) == 1


def test_adk_destroys_only_an_unnamed_machine(store: _Store) -> None:
    named = MachineSession(machine_name="keep-me", client=store)  # type: ignore[arg-type]
    named.get_machine()
    asyncio.run(named.close())
    assert ("destroy", "m-1") not in store.calls

    unnamed = MachineSession(client=store)  # type: ignore[arg-type]
    assert unnamed.machine_name.startswith("adk-")
    m = unnamed.get_machine()
    asyncio.run(unnamed.close())
    assert ("destroy", m.id) in store.calls


def test_adk_tools_return_structured_results(store: _Store) -> None:
    store.exec_handler = lambda kw: ExecResponse(stdout="hi\n", stderr="", exit_code=0)
    session = MachineSession(machine_name="m", client=store)  # type: ignore[arg-type]
    res = session.execute_command("echo hi")
    assert res == {"success": True, "stdout": "hi\n", "stderr": "", "exit_code": 0, "machine": "m"}

    store.exec_handler = lambda kw: ExecResponse(stdout="", stderr="boom\n", exit_code=2)
    failed = session.execute_command("false")
    assert failed["success"] is False and failed["exit_code"] == 2 and failed["stderr"] == "boom\n"


def test_adk_code_and_files_round_trip_through_base64(store: _Store) -> None:
    written: dict[str, str] = {}

    def handler(kw: dict[str, Any]) -> ExecResponse:
        cmd = kw["cmd"]
        if "base64 -d >" in cmd:
            written["body"] = base64.b64decode(cmd.split("printf %s '")[1].split("'")[0]).decode()
            return ExecResponse(stdout="", stderr="", exit_code=0)
        if cmd.startswith("base64 -w0"):
            return ExecResponse(stdout=base64.b64encode(b"file body").decode(), stderr="", exit_code=0)
        if "python3 -" in cmd:
            written["code"] = base64.b64decode(cmd.split("printf %s '")[1].split("'")[0]).decode()
            return ExecResponse(stdout="ran\n", stderr="", exit_code=0)
        return ExecResponse(stdout="", stderr="", exit_code=0)

    store.exec_handler = handler
    session = MachineSession(machine_name="m", client=store)  # type: ignore[arg-type]

    assert session.write_file("/app/x.py", "print('hi')\n") == {"success": True, "path": "/app/x.py", "bytes": 12}
    assert written["body"] == "print('hi')\n"
    assert session.read_file("/app/x.py")["content"] == "file body"
    assert session.execute_code("print('hi')")["stdout"] == "ran\n"
    assert written["code"] == "print('hi')"
    assert session.execute_code("...", language="cobol")["success"] is False
    # The size cap keeps a doomed argv off the wire.
    big = session.write_file("/big", "x" * (MAX_WRITE_BYTES + 1))
    assert big["success"] is False and "larger than" in big["error"]


def test_adk_restore_refuses_without_confirmation(store: _Store) -> None:
    session = MachineSession(machine_name="m", client=store)  # type: ignore[arg-type]
    session.get_machine()
    ck = session.create_checkpoint("pre-upgrade")
    assert ck["success"] and ck["checkpoint_id"] == "ck-1"
    refused = session.restore_checkpoint(ck["checkpoint_id"])
    assert refused["success"] is False and "confirm=true" in refused["error"]
    assert ("restore", "ck-1") not in store.calls
    done = session.restore_checkpoint(ck["checkpoint_id"], confirm=True)
    assert done["success"] and ("restore", "ck-1") in store.calls
    listed = session.list_checkpoints()
    assert [c["checkpoint_id"] for c in listed["checkpoints"]] == ["ck-1"]


def test_adk_plugin_exposes_the_seven_tools() -> None:
    pytest.importorskip("google.adk", reason="the adk extra is not installed")
    plugin = PilotsPlugin("k", "m", base_url="http://127.0.0.1:1")
    names = [t.name for t in plugin.get_tools()]
    assert sorted(names) == sorted(TOOL_NAMES)
    for tool in plugin.get_tools():
        assert len(tool.description) > 40


# ---------------------------------------------------------------------------
# OpenAI Agents SDK
# ---------------------------------------------------------------------------


def test_openai_agents_session_maps_workspace_and_owns_what_it_created(store: _Store) -> None:
    client = PilotsSandboxClient(client=store)  # type: ignore[arg-type]
    session = client.create_sync()
    assert session.id.startswith("agents-") and session.owned is True
    assert session.workspace_root == "/home/pilot/workspace"
    # The workdir is created on the way up.
    assert any("mkdir -p" in c[1] for c in store.calls)

    store.exec_handler = lambda kw: ExecResponse(stdout=kw["cwd"], stderr="", exit_code=0)
    assert session.exec_sync("pwd").stdout == "/home/pilot/workspace"
    assert session.exec_sync("pwd", cwd="/workspace/sub").stdout == "/home/pilot/workspace/sub"
    assert session.exec_sync("pwd", cwd="/etc").stdout == "/etc"

    session.close_sync()
    assert ("destroy", session.machine_id) in store.calls


def test_openai_agents_attaches_without_owning(store: _Store) -> None:
    client = PilotsSandboxClient(client=store)  # type: ignore[arg-type]
    first = client.create_sync(PilotsSandboxClientOptions(machine_name="mine"))
    assert first.owned is False
    second = client.create_sync(PilotsSandboxClientOptions(machine_name="mine"))
    assert second.machine_id == first.machine_id
    assert len([c for c in store.calls if c[0] == "create"]) == 1
    second.close_sync()
    assert not any(c[0] == "destroy" for c in store.calls)
    assert asyncio.run(client.resume("nope")) is None


def test_openai_agents_snapshot_and_restore(store: _Store) -> None:
    client = PilotsSandboxClient(client=store)  # type: ignore[arg-type]
    session = client.create_sync()
    ck = session.snapshot_sync("before")
    assert ck == "ck-1"
    session.restore_sync(ck)
    assert ("restore", "ck-1") in store.calls
    assert session.capabilities["snapshots"] and session.capabilities["fork"] is False


def test_openai_agents_files(store: _Store) -> None:
    written: dict[str, str] = {}

    def handler(kw: dict[str, Any]) -> ExecResponse:
        cmd = kw["cmd"]
        if "base64 -d >" in cmd:
            written["body"] = base64.b64decode(cmd.split("printf %s '")[1].split("'")[0]).decode()
            return ExecResponse(stdout="", stderr="", exit_code=0)
        if cmd.startswith("base64 -w0"):
            return ExecResponse(stdout=base64.b64encode(b"contents").decode(), stderr="", exit_code=0)
        if cmd.startswith("ls -1A"):
            return ExecResponse(stdout="a\nb\n", stderr="", exit_code=0)
        return ExecResponse(stdout="", stderr="", exit_code=0)

    store.exec_handler = handler
    session = PilotsSandboxClient(client=store).create_sync()  # type: ignore[arg-type]
    session.write_sync("/workspace/main.py", "print(1)\n")
    assert written["body"] == "print(1)\n"
    assert session.read_sync("main.py") == b"contents"
    assert session.listdir_sync() == ["a", "b"]


# ---------------------------------------------------------------------------
# Claude Managed Agents
# ---------------------------------------------------------------------------


def test_managed_agents_runs_a_session_in_a_machine_and_cleans_up(store: _Store) -> None:
    config = WorkerConfig(environment_id="env_1", environment_key="key_1")
    runner = MachineRunner(config, client=store)  # type: ignore[arg-type]
    item = WorkItem(work_id="w-1", session_id="sess_ABCDEFGH1234", secret="s3cret")

    store.exec_handler = lambda kw: ExecResponse(stdout="done", stderr="", exit_code=0)
    result = runner.run_item(item)

    assert result["exit_code"] == 0 and result["session_id"] == "sess_ABCDEFGH1234"
    assert result["machine"] == "agent-abcdefgh1234"
    # The runner command ran with the session's environment.
    ran = [c for c in store.calls if c[0] == "exec" and c[1] == DEFAULT_RUNNER]
    assert len(ran) == 1
    env = store.exec_env[-1]
    assert env["ANTHROPIC_WORK_ID"] == "w-1"
    assert env["ANTHROPIC_SESSION_ID"] == "sess_ABCDEFGH1234"
    assert env["ANTHROPIC_WORK_SECRET"] == "s3cret"
    assert env["ANTHROPIC_ENVIRONMENT_ID"] == "env_1"
    # An ephemeral session leaves nothing behind.
    assert any(c[0] == "destroy" for c in store.calls)
    assert store.machines_by_id == {}


def test_managed_agents_keeps_a_named_machine_and_can_checkpoint(store: _Store) -> None:
    config = WorkerConfig(environment_id="env_1", environment_key="key_1", machine_name="my-agent",
                          checkpoint_before_session=True)
    runner = MachineRunner(config, client=store)  # type: ignore[arg-type]
    runner.run_item(WorkItem(work_id="w-1", session_id="s-1"))
    runner.run_item(WorkItem(work_id="w-2", session_id="s-2"))
    assert len([c for c in store.calls if c[0] == "create"]) == 1
    assert not any(c[0] == "destroy" for c in store.calls)
    assert [c[1] for c in store.calls if c[0] == "checkpoint"] == ["ck-1", "ck-2"]


def test_managed_agents_reports_a_failure_instead_of_raising(store: _Store) -> None:
    from pilots.errors import PilotsError

    def boom(kw: dict[str, Any]) -> ExecResponse:
        raise PilotsError("the machine went away", status=404, code="not_found", next="create it again")

    store.exec_handler = boom
    runner = MachineRunner(WorkerConfig(environment_id="e", environment_key="k"), client=store)  # type: ignore[arg-type]
    result = runner.run_item(WorkItem(work_id="w", session_id="s"))
    assert result["code"] == "not_found" and "went away" in result["error"]
    # The machine is still cleaned up.
    assert any(c[0] == "destroy" for c in store.calls)
