#!/usr/bin/env python3
"""Ask before a pilots MCP call that changes what is live or throws work away.

Claude Code hands the hook the tool call as JSON on stdin. A safe call prints
nothing and Claude's ordinary permission policy applies. A risky one prints a
PreToolUse `ask` decision, so the user sees the exact call before it runs.

The list is keyed on the tool names the fleet toolset registers (agents/mcp).
A tool that is not named here is not guarded; that is the correct default for
a read, and the wrong one for a new destructive tool, which is why the test in
agents/mcp checks every destructive tool is listed.
"""

from __future__ import annotations

import json
import re
import sys
from typing import Any

# name -> why the user should look before it runs
DESTRUCTIVE_TOOLS = {
    "destroy_machine": (
        "Destroying a machine deletes its disk, checkpoints and URL. There is no "
        "undo. Confirm the exact machine name or id."
    ),
    "restore": (
        "Restoring a checkpoint discards every change made after it, in place, "
        "on the same machine. Confirm the machine and the checkpoint id."
    ),
    "rollback": (
        "Rolling back changes what the service's URL serves right now. Confirm "
        "the service and which release it will return to."
    ),
}

# Commands worth a checkpoint first: package upgrades, migrations, bulk
# deletion. Matched case-insensitively against the exec command.
CHECKPOINT_FIRST = (
    r"\brm\s+-rf\b",
    r"\bdrop\s+(database|table)\b",
    r"\btruncate\s+table\b",
    r"\bdelete\s+from\b",
    r"\b(db:)?migrate\b",
    r"\bprisma\s+migrate\b",
    r"\balembic\s+upgrade\b",
    r"\bdrizzle-kit\s+(push|migrate)\b",
    r"\bnpm\s+(install|update|audit\s+fix)\b",
    r"\bpnpm\s+(install|update|add|up)\b",
    r"\byarn\s+(install|upgrade|add)\b",
    r"\bpip\s+install\b",
    r"\buv\s+(pip\s+)?(install|sync)\b",
    r"\bapt(-get)?\s+(install|upgrade|dist-upgrade)\b",
    r"\bapk\s+add\b",
    r"\bdnf\s+(install|upgrade)\b",
)


def read_payload() -> dict[str, Any]:
    try:
        raw = sys.stdin.read()
        value = json.loads(raw) if raw.strip() else {}
        return value if isinstance(value, dict) else {}
    except json.JSONDecodeError:
        return {}


def tool_basename(name: str) -> str:
    return name.rsplit("__", maxsplit=1)[-1].lower()


def ask(reason: str) -> None:
    print(
        json.dumps(
            {
                "hookSpecificOutput": {
                    "hookEventName": "PreToolUse",
                    "permissionDecision": "ask",
                    "permissionDecisionReason": reason,
                }
            }
        )
    )


def exec_command(tool_input: dict[str, Any]) -> str:
    # exec takes argv; exec_stream takes argv too. A string `cmd` is what a
    # sprites-shaped caller sends. Any of them, joined, is what we scan.
    argv = tool_input.get("argv")
    if isinstance(argv, list):
        return " ".join(str(a) for a in argv)
    for key in ("cmd", "command"):
        value = tool_input.get(key)
        if isinstance(value, str):
            return value
    return json.dumps(tool_input, sort_keys=True)


def main() -> int:
    payload = read_payload()
    name = tool_basename(str(payload.get("tool_name", "")))
    tool_input = payload.get("tool_input")
    if not isinstance(tool_input, dict):
        tool_input = {}

    if name in DESTRUCTIVE_TOOLS:
        ask(DESTRUCTIVE_TOOLS[name])
        return 0

    if name in ("exec", "exec_stream"):
        command = exec_command(tool_input).lower()
        if any(re.search(p, command) for p in CHECKPOINT_FIRST):
            ask(
                "This command makes broad, persistent changes inside the machine. "
                "Take a checkpoint first (the `checkpoint` tool) so it can be undone "
                "with `restore`, or confirm that one already exists."
            )

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
