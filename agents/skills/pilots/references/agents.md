# Connecting an agent to pilots

Two ways in, and the difference is what the agent can reach.

## Hosted: every host serves `/mcp`

```
pilot login
pilot mcp install <harness>
```

`pilot mcp install --list` names the harnesses it knows: `claude-code`,
`codex`, `cursor`, `vscode`, `opencode`, `windsurf`, `gemini`, `zed`,
`claude-desktop`, and `generic` for anything else. Each writes the server
entry into that agent's own config file, in that agent's own shape, merged
with what is already there. `--project` writes the repository-scoped file
instead of the user-wide one, and `--print` shows the entry without writing.

Any MCP client can also be pointed at the URL by hand:

```json
{ "mcpServers": { "pilots": { "type": "http", "url": "https://api.pilotrun.app/mcp",
  "headers": { "Authorization": "Bearer $PILOT_API_KEY" } } } }
```

The endpoint is Streamable HTTP, stateless, on every host of the fleet. There
is no gateway and no session pinned to one machine, so a client that reconnects
to a different host loses nothing.

**Twenty tools**, everything that needs only the API: `create_machine`,
`list_machines`, `status`, `exec`, `exec_stream`, `logs`, `checkpoint`,
`restore`, `promote`, `destroy_machine`, `build_logs`, `list_services`,
`service`, `releases`, `rollback`, `domains`, `volumes`, `diagnose`, `init`,
`docs`.

## Stdio: `pilot mcp`, and six more tools

```
pilot mcp install <harness> --stdio
```

Runs the server as a local process, which adds the tools that read the agent's
own filesystem: `deploy` (a directory to a URL in one call), `build`, `plan`,
`generate_dockerfile`, `push_file` and `pull_file`. Use this one when the agent
is working in a checkout. The hosted server's `init` tool says so too, so an
agent that finds `deploy` missing is told where it is rather than guessing.

## Logging in without a token

A client that dials the URL with no credential gets a 401 naming
`/.well-known/oauth-protected-resource`, which names the dashboard as the
authorization server. The client registers itself, opens a browser, and the
person approves it on a consent screen. What comes back is an ordinary pilots
token, so nothing on the fleet learns a second credential type and revoking it
on the tokens page stops it everywhere.

The consent screen offers three restrictions, and the fleet enforces all three:

| Restriction | What it does |
| --- | --- |
| name prefix | Every machine and service this token names must start with it. The default is `mcp-`. |
| machine cap | How many machines with that prefix may exist at once, counted at create time. |
| expiry | When the token stops authenticating, checked on every request. |

A refusal says which one was hit and what would work instead: a name outside
the prefix comes back with the prefixed name spelled out.

## The Claude Code plugin

```
/plugin marketplace add vivek7405/pilots
/plugin install pilots@pilots
```

Adds the hosted server, this skill, `/pilots:status`, `/pilots:smoke`, and a
hook that asks before `destroy_machine`, `restore` and `rollback`, and before
an `exec` that ought to be checkpointed first (a migration, a package upgrade,
a recursive delete). Cursor, Codex, Copilot and Kiro read the portable Agent
Plugins v1 manifest in the same directory.

## SDKs

| Language | Install | Notes |
| --- | --- | --- |
| TypeScript | `npm i @pilots/sdk` | Zero dependencies. Also `@pilots/sdk/tanstack` and `@pilots/sdk/sprites-compat`. |
| Python | `pip install pilots-sdk` | Extras: `[adk]`, `[openai-agents]`, `[anthropic]`. Also `pilots.sprites_compat`. |
| Go | `go get github.com/vivek7405/pilots/sdks/go` | One dependency. |
| Elixir | `{:pilots, "~> 0.1"}` | One dependency. HTTP through OTP's own `:httpc`. |

The first three keep their own copy of the wire types, and each has a test
that parses hostd's Go source and fails when the two disagree, so a field the
platform added cannot go missing from a client. The Elixir one returns plain
maps with the server's own keys, so its drift test checks the route table
instead: a path it calls that hostd no longer serves fails the build.

## Framework adapters

- **Google ADK**: `pilots.adk.PilotsPlugin` gives an agent seven tools, with a
  restore that refuses without an explicit confirmation.
- **OpenAI Agents SDK**: `pilots.openai_agents.PilotsSandboxClient`, an
  ephemeral machine per session or a named one that outlives it.
- **Claude Managed Agents**: `pilots.managed_agents.run_worker`, which runs
  each session's tool calls inside a machine you own.
- **TanStack AI**: `pilotsSandbox()` from `@pilots/sdk/tanstack`.

## The editor

The VS Code extension mounts a machine as a workspace folder
(`pilot://<name>/<path>`) and opens a shell in it. Every read and write is a
command over the exec route, so there is nothing to install in the guest.
