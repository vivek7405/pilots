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
| `packages/cli/` | `pilot` CLI |
| `sdks/js/`, `sdks/go/` | `@pilots/sdk` (npm) and `github.com/vivek7405/pilots/sdks/go` |
| `scripts/` | one-shot bash: host bootstrap, golden rootfs, e2e battery |

Architecture: `ARCHITECTURE.md`.
Run it on one machine: `docs/local.md`.
