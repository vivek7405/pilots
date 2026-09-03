# AGENTS.md — pilots monorepo

Canonical guide for any agent or human working in this repo. It owns the
**workflow rules**. `CLAUDE.md` is `@AGENTS.md` so both load automatically.

> **Read `ARCHITECTURE.md` before writing any code.** It is the complete,
> self-contained design: the invariants there are load-bearing and several
> were paid for with production incidents in the predecessor codebase.

---

## The bar (read this first, before planning or implementing anything)

Every plan written into an issue and every line of code is measured against
this, in order. It is loaded into context automatically (`CLAUDE.md` is
`@AGENTS.md`), so it applies while planning AND while implementing.

1. **At par with e2b-infra, fly.io and sprites.dev, or better.** Never below
   any of them on a capability they have. `docs/prior-art/README.md` carries
   the scorecard; a design that lands a "~" or "✗" where a competitor has "✓"
   must say why.
2. **Resilient, robust, and simple to maintain.** Three processes per host,
   no extra tiers, no second copy of a contract, no dependency added for
   convenience. When two designs are equally fast, the one with fewer moving
   parts wins.
3. **No central control plane, on purpose.** The shape is borrowed from
   uncloud (`docs/prior-art/uncloud.md`): every host runs the identical stack
   and serves the full API from its local Corrosion replica. No request path
   may depend on any specific host, including the dashboard's. A design that
   needs a coordinator has already failed this bar.
4. **Deploying is very fast, and a webjs app deploys fastest of all.** webjs
   is our own buildless, web-components, full-stack framework: its deploy is
   `npm install` on the fleet-wide layer cache, then a restore from the
   release snapshot, then `/__webjs/ready`. Nothing may sit between those
   steps. See `docs/prior-art/webjs-deploy-contract.md`. **Every web app in
   this repo is a webjs app**: the dashboard, the marketing site, anything
   that serves a page. No other web framework, bundler or UI library.
5. **Spin-up, pause/resume and snapshot/restore are extremely fast.** Create
   is a restore, not a boot; wake is a held request, never a waiting page;
   a checkpoint's resume gap is independent of machine size. The numbers are
   the SLO table in #7 and the levers beyond it are #22; a change that makes
   any of them slower is a regression even if every test stays green.

When a phase plan, an issue body, or a review comment conflicts with one of
these, this section wins and the conflict is stated in the issue.

## What pilots is

A **2-in-1 sandbox + PaaS** on Firecracker microVMs — instant sandboxes for
AI agents and durable production services, on **one primitive**. Feature
parity with Fly.io, sprites.dev, and e2b-infra.

What makes it different:

- **No central control plane.** Every host runs the identical stack and
  serves the full API. No scheduler tier, no managed database, no load
  balancer appliance. No request path may require a specific machine to be
  alive.
- **Bare-metal native.** Hetzner dedicated hosts, not a cloud IaC shape.
  Adding a host is `scripts/host-bootstrap.sh <ip>`.
- **One primitive, two faces.** A sandbox and a production service are the
  same `machine` with different lifecycle knobs. `promote` turns one into
  the other without changing its URL.

Tracking: [project board](https://github.com/users/vivek7405/projects/10).
Master plan and phase breakdown: issues #1–#7.

## Layout

```
apps/hostd/        Go — the entire per-host data plane (own go.mod)
apps/dashboard/    webjs — accounts, API keys, UI (Phase 6)
packages/cli/      `pilot` CLI (Phase 6)
sdks/js, sdks/go   typed clients (Phase 6)
scripts/           one-shot bash + the e2e battery
ARCHITECTURE.md    the design; source of truth
```

`apps/hostd` is a separate Go module inside the npm workspace — the npm
workspaces never see it. Run Go commands from `apps/hostd/`.

---

## Hard rules

### Architecture invariants (never violate without changing ARCHITECTURE.md first)

1. **Single-writer.** A host writes ONLY rows describing its own machines.
   The sanctioned exceptions are deterministic-owner operations (name
   allocation and self-heal claims of a *provably dead* host's machines) and
   the write-once rows in `tenancy` and `api_key_revocations`, plus
   `api_keys` and `org_quotas` on an admin-scoped request. Violating this
   does not error — it corrupts state silently through CRDT merges. A row is
   only safe for "any host" to write when it is written once, or has one
   logical writer: then the merge has nothing to corrupt.
2. **The data plane never depends on the control plane.** Routing and wake
   read local state only. If that is not true of a change, the change is
   wrong.
3. **S3 is the only truth for machine state.** Local NVMe is a cache. The
   test: wipe any host's disk; nothing is lost.
4. **URLs are permanent.** A machine's URL survives suspend, wake,
   checkpoint, restore, promote, redeploy, and host migration. Any change
   that can mint a new URL for an existing machine is a bug.
5. **Snapshots are host-agnostic.** Nothing host-specific may enter a
   snapshot — that is why guest networking uses constant addresses and the
   rootfs is bind-mounted to a constant path. See ARCHITECTURE.md.
6. **Never `ALTER TABLE` a live Corrosion table, and never add a column to
   one that has rows.** cr-sqlite backfills every existing row on a column
   add and gossips the backfill; that took fly's fleet down twice for
   ~11.5 h (`docs/prior-art/fly-io.md` §10, 2024-11-25). A shape change is a
   new table plus a dual read, and a table that has ever held a row is
   closed to column adds (see `internal/state/schema.sql:144-150`). The only
   column adds in this repo are in `state.go`'s `addMissingColumns`, for
   SQLite hosts, against tables that carried no rows.

### Workflow

- **One task per git worktree.** `git worktree add -b feat/<slug> ../pilots-<slug> main`.
  Never share a checkout between concurrent tasks.
- **Never commit on `main`.** Feature branch + PR, always.
- **Commit per logical unit, push after each.** Do not batch unrelated work
  into one commit.
- **Imperative commit subjects under 72 chars.** The body explains *why*,
  not *what* — the diff shows what.
- **No AI attribution** in commits: no `Co-Authored-By: Claude`, no
  "Generated by", no similar trailers.
- **Run checks before committing:** from `apps/hostd/`, `gofmt -l .` (it must
  print nothing), `go vet ./...` and `go test ./...`; from the root,
  `npm test`. These are exactly what CI runs -- gofmt was missing from this
  list while CI enforced it, so a branch could be green locally and red on
  main. Note gofmt reformats DOC comments as well as code: it turns `''` and
  ``` `` ``` into curly quotes, which quietly mangles a comment that meant an
  empty SQL string. Also run `bash -n` on every shell script you edited and
  `node --check scripts/e2e.mjs` — a syntax error in either is only found at
  the moment it is needed, on a host, mid-bootstrap.
- Every phase issue (#2–#7) carries a **gate checklist**. An issue closes
  when its gate is green, not when the code is written.

### Testing

- `scripts/e2e.mjs` is the single, **monotonically growing** battery. Later
  phases add assertions; they never retire earlier ones.
- It drives the **public API only** — the same surface an SDK or agent uses.
  A green run therefore exercises routing + hostd + Firecracker together.
- It skips cleanly (exit 0) unless `PILOTS_E2E=1`, so `npm test` stays green
  on machines with no KVM.
- Its API key comes from `hostd bootstrap-key` and must carry `admin`: the
  battery drives routes from every scope, and a narrower key turns real
  assertions into 403s.
- `scripts/cluster/gate.sh` is the fleet battery, a numbered `say` section per
  property, run against the local multi-node rig. It grows monotonically too.

**Where a new test belongs** — the split is what can *observe* the assertion:

| The assertion needs… | It goes in |
|---|---|
| only the public API (`/v1/...`, `/metrics`, an SDK, the CLI) | `scripts/e2e.mjs` |
| a host shell — `/sys`, `/proc`, cgroup files, `journalctl`, `kill -9 hostd`, a reboot | `scripts/cluster/gate.sh`, as a new numbered section |

A hostility test usually has both halves, and they are written as a pair: the
e2e half asserts what a client would see, the gate half asserts that the host
kept none of the wreckage. Neither half may retire an assertion, and neither
may *skip* one — a block that cannot set itself up fails loudly, because a
quiet early return retires every assertion below it at runtime.
- The **metal tier** runs only under `PILOTS_E2E_METAL=1` on a host whose
  `/v1/health` reports `reflink: true`. It replaces the laptop budgets with
  the SLOs the product is sold on; the flag on a host that cannot share
  extents fails the run rather than downgrading it.

---

## Working on the engine

Reach for `~/Documents/Projects/sandbox/pilots-old` as an **answer key**: a
predecessor that solved the same Firecracker problems and paid for the
lessons now recorded in ARCHITECTURE.md. Read it; do not build on it and do
not import it. Its control plane (central Postgres, SSH orchestration) is
exactly what this repo exists to replace.

Two components are explicitly **ports, not rewrites** — they encode kernel
ABI details that are expensive to rediscover:

- the **uffd handler** (`cmd/pilot-uffd-handler`, ~881 LOC)
- the **NBD handler** (`cmd/pilot-nbd-handler`, ~560 LOC)

### e2b-infra: read it BEFORE the second attempt

`~/Documents/Projects/sandbox/e2b-infra` is a complete, open-source (Apache-2.0)
implementation of this product's hard half, cloned locally. Every line is
there. **When you are stuck, about to guess, or brainstorming an approach, go
read it first** -- not after two rounds of trial and error. That ordering is
the rule, and it is a rule because the alternative has already cost time here:
two round trips were spent rediscovering Alpine's non-usr-merged `/sbin` and
the OpenRC inittab swap, both of which
`packages/orchestrator/pkg/template/build/phases/base/provision.sh` handles
outright.

Where the answers live:

| Question | Look at |
|---|---|
| microVM lifecycle, create/pause/resume | `packages/orchestrator/pkg/sandbox/` |
| userfaultfd handling | `packages/orchestrator/pkg/sandbox/uffd/` |
| block devices, chunking, caching | `packages/orchestrator/pkg/sandbox/block/` |
| netns, tap, addressing | `packages/orchestrator/pkg/sandbox/network/` |
| guest image fixups (init, `/sbin`, resolv.conf) | `packages/orchestrator/pkg/template/build/phases/base/provision.sh` |
| how to MEASURE snapshot/restore | `packages/orchestrator/benchmarks/benchmark_test.go` (`BenchmarkBaseImageLaunch`, cycles `start-and-pause` and `start-pause-resume`) |
| making the pause window small | `packages/orchestrator/cmd/resume-build/fph_bench.go` -- pause time vs memfile size, free page reporting (FPR) and free page hinting (FPH) |
| what a build artifact contains | `packages/orchestrator/cmd/inspect-build/`, `cmd/copy-build/` -- note a build carries a **memfile**, which is why a deployed app there RESTORES instead of booting |

Two caveats, both the same shape as the `pilots-old` one:

- **Take mechanics, not architecture.** e2b has a central control plane -- an
  API tier over Postgres, with placement decided centrally. That is precisely
  what this repo exists to replace. Copy how they touch the kernel, never how
  they decide who runs what.
- **It ships no benchmark numbers.** The harness is committed; no threshold,
  SLO, or millisecond figure is. Do not cite a latency target as coming from
  the repo -- run the harness, or find their published figures.

For what the competition actually MEASURES at, `pilots-old` carries empirical
sprites.dev figures taken by driving its CLI rather than read off a blog:
`apps/cloud/host-scripts/PHASE3-NOTES.md` (the warm/cold tier table) and the
reference note it points to. Summarised, so the target is concrete:

| sprites.dev | measured |
|---|---|
| warm resume (snapshot on local NVMe) | ~150ms |
| cold resume (snapshot from object storage) | ~775ms |
| create (from a pre-warmed pool) | ~2s |
| checkpoint create (CoW metadata, no data copy) | <1s |
| checkpoint restore (data fetched from S3) | ~8-10s |

Their warm tier exists for two reasons, and the second is the one that is easy
to miss: writing a 2GiB snapshot to a remote object store over 1Gbit/s takes
~16s against ~2s to local NVMe, so the local tier is the fast WRITE target as
much as the fast read one.

### Prior art: `docs/prior-art/` — read the relevant note before designing

The bar is "at par with fly.io, sprites.dev and e2b-infra, or better", so every
design decision starts from what they did and why. `docs/prior-art/` holds
deep, sourced notes (URL or `path:line` on every claim; inference marked) on
each system, plus a synthesis. Start at `docs/prior-art/README.md` — it has the
capability scorecard and the "which note answers which question" index. Then
read only the note you need:

| Working on… | Read first |
|---|---|
| placement, self-heal, Corrosion schema/ops, router LB, rollout, incidents | `fly-io.md` (flyd/flaps split, the three 2024 Corrosion outages) |
| checkpoints, sleep/wake tiers, exec frames, sandbox env, agent DX | `sprites-dev.md` (crisp runs there today; the surface to match) |
| uffd, memfile/diff chain, block/chunk store, templates, envd, netns | `e2b-infra.md` (mechanics; the AGENTS.md table above is its short index) |
| Corrosion join/sync gates, own-rows discipline, WG mesh, `.internal` DNS | `uncloud.md` (where the no-control-plane shape was borrowed from) |
| deploy path, readiness gate, rollout defaults for the primary workload | `webjs-deploy-contract.md` |

The COPY / REJECT / parity sections at the end of each note are the actionable
part; an issue that contradicts a REJECT entry must say why.
