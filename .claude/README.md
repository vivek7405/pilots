# .claude/ and .agents/ — for working ON pilots

Config and skills for an agent **developing this repo**. Not shipped.

Do not confuse them with the two directories next door, which are product:

| Path | What it is | Ships? |
|---|---|---|
| `.claude/`, `.agents/` | how to develop pilots: the rig, the board ids | no |
| `agents/` | the agent package pilots SELLS: the skill, the Claude Code plugin, the portable Agent Plugin, the MCP toolset | yes |
| `.claude-plugin/` | the marketplace manifest that publishes `agents/` | yes |

A change to how a pilots USER's agent behaves belongs in `agents/`. A change
to how an agent working on this checkout behaves belongs here.

## Why both directories, and where the real file is

Different agents look in different places, and the answer to that is a
symlink rather than a copy. The convention is the one the webjs repo already
uses, so the two repos work the same way:

- **A skill's real content lives in `.claude/skills/<name>/SKILL.md`.**
- **`.agents/skills/<name>` is a symlink to it**, so an agent that reads
  `.agents/` finds the same file rather than a second version of it that will
  drift.
- **`.agents/rules/workflow.md` is a real file**, not a link, because some
  agents load that directory and nothing else. It says plainly that
  `AGENTS.md` is the full contract, and carries only the subset that is
  expensive to violate.

`AGENTS.md` at the root stays the one contract, and `CLAUDE.md` is one line
pointing at it. Nothing here restates it.

## Contents

- `skills/rig/SKILL.md` — the three-host cluster: how to reach it, how to put
  a build on it, how to run the batteries, and the traps that have each cost
  a multi-hour run at least once.
- `gh-ids.env` — GitHub project board node ids, so a board skill does not
  spend GraphQL budget rediscovering constants. **No credentials, ever**; the
  file says why.
