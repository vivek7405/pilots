# .claude/ — for working ON pilots

Config and skills for an agent **developing this repo**. Not shipped.

Do not confuse it with the two directories next door, which are product:

| Path | What it is | Ships? |
|---|---|---|
| `.claude/` | how to develop pilots: the rig, the board ids | no |
| `agents/` | the agent package pilots SELLS: the skill, the Claude Code plugin, the portable Agent Plugin, the MCP toolset | yes |
| `.claude-plugin/` | the marketplace manifest that publishes `agents/` | yes |

A change to how a pilots USER's agent behaves belongs in `agents/`. A change
to how an agent working on this checkout behaves belongs here.

## Why there is no `.agents/`

The portable, cross-agent contract for this repo is `AGENTS.md` at the root,
and `CLAUDE.md` is one line pointing at it, so every agent loads the same
file. A `.agents/` directory here would be a second copy of that contract,
which is the thing bar 2 exists to prevent. If a skill here turns out to be
worth running from a non-Claude agent, move it to `agents/skills/` where the
portable ones already live, rather than forking the rules.

## Contents

- `skills/rig/SKILL.md` — the three-host cluster: how to reach it, how to put
  a build on it, how to run the batteries, and the traps that have each cost
  a multi-hour run at least once.
- `gh-ids.env` — GitHub project board ids, so a skill does not spend GraphQL
  budget rediscovering constants.
