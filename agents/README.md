# `agents/`: pilots for agents

Everything an agent needs to use pilots, in one directory that is three
packages at once:

- **A Claude Code plugin.** `.claude-plugin/plugin.json`, `.mcp.json` (the
  hosted MCP server), `hooks/hooks.json` with `scripts/guard.py` (asks before
  `destroy_machine`, `restore`, `rollback`, and before an `exec` that should be
  checkpointed first), and the skills under `skills/`: `pilots` (loaded by the
  model when the task is about pilots), `status` (`/pilots:status`) and
  `smoke` (`/pilots:smoke`).
- **A portable [Agent Plugins v1](https://agent-plugins.org/specification)
  package.** `plugin.json`, `mcp.json` and the same `skills/`, for Cursor,
  Codex, GitHub Copilot, Kiro and every other client that loads that format.
- **A Go module** (`github.com/vivek7405/pilots/agents`) that embeds the skill
  and holds the MCP toolset in `mcp/`. hostd serves the toolset at `/mcp` on
  every host and `pilot mcp` serves it on stdio; both read the pages from
  this embed, so the plugin, the CLI and the fleet ship the same words.

## Install

Claude Code:

```
/plugin marketplace add vivek7405/pilots
/plugin install pilots@pilots
```

Then set `PILOT_API_KEY` (and `PILOT_API_URL` for a self-hosted fleet) in the
environment Claude Code starts from, or run `pilot mcp install claude-code`,
which writes the server entry with the key `pilot login` stored. `/pilots:status`
checks the result.

Any other MCP client: point it at `https://api.pilotrun.app/mcp` with
`Authorization: Bearer <key>`, or run `pilot mcp install <harness>`; `pilot mcp
install --list` names the harnesses it knows.

## The skill

`skills/pilots/SKILL.md` and its `references/` are the one copy of the agent
docs. `pilot init` copies them into a repository's `.agents/skills/pilots/`,
`pilot skill install` links them into `~/.claude/skills/pilots`, and the MCP
servers expose them as `pilots-docs://` resources and through the `docs` tool.
Edit them here; nothing else holds a copy.
