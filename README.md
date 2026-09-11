# pilots

**The 2-in-1 sandbox + PaaS on Firecracker microVMs.** Instant sandboxes for
AI agents, durable production services on the same primitive. No central
control plane. Every host runs the identical stack and serves the full API.
Bare-metal native (Hetzner). One command promotes a sandbox to production
with its URL unchanged.

Monorepo:

| Path | What |
|---|---|
| `apps/hostd/` | Go, the entire per-host data plane (FC lifecycle, router, TLS, wake, snapshots, self-heal) |
| `apps/dashboard/` | webjs: accounts, API keys, UI (deployed on pilots itself) |
| `apps/website/` | webjs: the marketing site |
| `apps/pilot/` | the `pilot` CLI and TUI, and the stdio MCP server |
| `apps/vscode/` | the VS Code extension: a machine as a workspace folder |
| `packages/cli/` | the previous TypeScript `pilot` CLI, kept until apps/pilot is at parity |
| `agents/` | the agent package: the skill (`SKILL.md` plus ten reference pages, embedded into hostd and `pilot`, served as `pilots-docs://` resources), the Claude Code plugin, the portable Agent Plugin, and the shared MCP toolset |
| `sdks/js/`, `sdks/go/`, `sdks/python/`, `sdks/elixir/` | `@pilots/sdk` (npm), `github.com/vivek7405/pilots/sdks/go`, `pilots-sdk` (PyPI) and `pilots` (Hex). Each has a drift test against hostd's own source |
| `scripts/` | one-shot bash: host bootstrap, golden rootfs, e2e battery |

Architecture: `ARCHITECTURE.md`.
Run it on one machine: `docs/local.md`.
