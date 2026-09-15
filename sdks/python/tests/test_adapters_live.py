"""The three framework adapters, against a real fleet.

``test_adapters.py`` drives them over a fake client, which is the right test for
the lifecycle logic and cannot see the one thing that actually breaks these: a
PIN DRIFT. ``google-adk`` renames a base class, ``openai-agents`` changes the
shape of a sandbox client, and every fake-client test stays green while the
adapter no longer loads at all in anybody's project.

So this one imports the frameworks, builds the real objects, and drives one
machine per adapter.

Gated twice, and deliberately:

* ``PILOTS_E2E=1`` and a key, because it creates real machines;
* the framework extra being installed, per adapter, so a contributor who
  installed one extra is not told the other two failed.

Run it from the rig procedure in ``docs/local.md``::

    PILOTS_E2E=1 PILOT_API_URL=... PILOT_API_KEY=... pytest tests/test_adapters_live.py
"""

from __future__ import annotations

import os
import uuid

import pytest

from pilots import PilotsClient

LIVE = os.environ.get("PILOTS_E2E") == "1"
API_KEY = os.environ.get("PILOT_API_KEY", "")

pytestmark = pytest.mark.skipif(
    not LIVE or not API_KEY,
    reason="needs PILOTS_E2E=1 and PILOT_API_KEY against a running fleet",
)


@pytest.fixture(name="client")
def _client() -> PilotsClient:
    return PilotsClient(API_KEY)


def _tag() -> str:
    return uuid.uuid4().hex[:8]


def _destroy(client: PilotsClient, machine_id: str) -> None:
    """Best effort, and it must never fail the test it is cleaning up after.

    A failed cleanup leaves a machine behind, which is a bill. A cleanup that
    RAISES hides whichever real assertion failed first, which is worse.
    """
    if not machine_id:
        return
    try:
        client.machines.destroy(machine_id)
    except Exception:  # noqa: BLE001 - see the docstring
        pass


def test_the_adk_plugin_still_builds_its_tools(client: PilotsClient) -> None:
    """The assertion a fake client cannot make: ADK's own types still fit.

    ``get_tools`` defers the ADK import to its own body, so constructing the
    plugin proves nothing. Calling it is what fails when a pin drifts.
    """
    pytest.importorskip("google.adk", reason="pip install 'pilots[adk]'")
    from pilots.adk import TOOL_NAMES, PilotsPlugin

    plugin = PilotsPlugin(API_KEY, f"adk-live-{_tag()}")
    try:
        tools = plugin.get_tools()
        names = {getattr(tool, "name", getattr(tool, "__name__", "")) for tool in tools}
        assert names == set(TOOL_NAMES), (
            "the built tools drifted from TOOL_NAMES, so an agent configured "
            f"against one of them would call a tool that is not there: {sorted(names)}"
        )

        # And the machine half, once, so this is a live test rather than an
        # import check wearing one's clothes.
        result = plugin.session.execute_command("echo adk-live")
        assert "adk-live" in str(result), result
    finally:
        _destroy(client, _machine_named(client, plugin.machine_name))


def test_the_openai_sandbox_session_runs_a_command(client: PilotsClient) -> None:
    """Create a session through the adapter, run one command, destroy it."""
    pytest.importorskip("agents", reason="pip install 'pilots[openai-agents]'")
    from pilots.openai_agents import PilotsSandboxClient, PilotsSandboxClientOptions

    sandbox = PilotsSandboxClient(client=client)
    session = sandbox.create_sync(
        PilotsSandboxClientOptions(machine_name=f"oai-live-{_tag()}", mem_mib=512)
    )
    try:
        result = session.exec_sync("echo openai-live")
        assert "openai-live" in str(getattr(result, "stdout", result))
    finally:
        _destroy(client, session.machine_id)


def test_the_managed_agents_runner_takes_one_item(client: PilotsClient) -> None:
    """The runner's machine half, end to end.

    No framework import: this adapter is ours, and what it needs proving
    against a real fleet is that its lifecycle works, not that somebody else's
    types still line up.
    """
    from pilots.managed_agents import MachineRunner, WorkerConfig, WorkItem

    name = f"worker-live-{_tag()}"
    runner = MachineRunner(
        WorkerConfig(environment_id="live", environment_key="live", machine_name=name),
        client=client,
    )
    try:
        machine = runner.ensure_machine(WorkItem(work_id="w1", session_id="session-live"))
        assert machine.name == name, f"the runner made {machine.name}, want {name}"
        # Found rather than recreated on a second call, or a worker would make
        # one machine per item and pay for every one of them.
        again = runner.ensure_machine(WorkItem(work_id="w2", session_id="session-live"))
        assert again.id == machine.id, "the runner created a second machine for the same session"
    finally:
        _destroy(client, _machine_named(client, name))


def _machine_named(client: PilotsClient, name: str) -> str:
    """The id of a machine by name, or empty.

    Needed because two of these adapters own their machine internally and
    expose only its name, and cleanup has to work whether or not the test got
    far enough for the adapter to have created one.
    """
    if not name:
        return ""
    try:
        for machine in client.machines.list():
            if machine.name == name:
                return machine.id
    except Exception:  # noqa: BLE001 - cleanup must not raise
        return ""
    return ""
