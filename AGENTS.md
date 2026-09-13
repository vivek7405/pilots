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

1. **At par with the best microVM platforms in production, or better.** Never
   below any of them on a capability they have. The prior-art repo carries the
   scorecard; a design that lands a "~" or "✗" where an established platform
   has "✓" must say why.
2. **Resilient, robust, and simple to maintain.** Three processes per host,
   no extra tiers, no second copy of a contract, no dependency added for
   convenience. When two designs are equally fast, the one with fewer moving
   parts wins.
3. **No central control plane, on purpose.** The shape is borrowed from
   uncloud, Apache-2.0 (`docs/prior-art/uncloud.md`): every host runs the identical stack
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
AI agents and durable production services, on **one primitive**. Full feature
parity with the established microVM platforms, on a simpler shape.

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
agents/            the agent package: the skill, the Claude Code plugin, the
                   portable Agent Plugin, and the MCP toolset both servers
                   register (own go.mod)
apps/hostd/        Go — the entire per-host data plane (own go.mod)
apps/dashboard/    webjs — the product UI: overview, services, machines, terminal,
                   usage, tokens, team, GitHub App surface
apps/pilot/        Go — the `pilot` CLI and `pilot tui`, one static binary on
                   sdks/go (own go.mod); the MCP server lives here too
packages/cli/      the previous TypeScript `pilot` CLI, kept until apps/pilot
                   is at parity and the swap is made; its skill/ pages are
                   the ones apps/pilot serves
apps/vscode/       the VS Code extension: a machine as a workspace folder
apps/nvim/         the Neovim plugin: the same, over a pilot:// URL scheme;
                   its guest commands are a port of the extension's, so a
                   fix to one is a fix owed to the other
sdks/js, go,       typed clients; each carries a drift test against
  python, elixir     apps/hostd/internal/api
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
   allocation and self-heal claims of a *provably dead* host's machines),
   a **planned handoff** (a LIVE host offering its own machine to another, on
   a drain — the offer row is write-once, written by the machine's current
   owner, and the claim is checked against it: right target, right current
   owner, newest offer, machine not running), and
   the write-once rows in `tenancy`, `api_key_revocations` and
   `repo_links`, plus
   `api_keys` and `org_quotas` on an admin-scoped request, plus the object-row
   side tables (`url_auth`, `broker_grants`), each written by the host that
   writes the object row it describes. Violating this
   does not error — it corrupts state silently through CRDT merges. A row is
   only safe for "any host" to write when it is written once, or has one
   logical writer: then the merge has nothing to corrupt. And never a claim
   before the join gate opens: a replica that has not caught up cannot tell a
   dead host from one it has applied nothing from, so it may act on its own
   rows and on the PRESENCE of a foreign row, never on the absence of one
   (`internal/state/corrosion/joingate.go`, ARCHITECTURE.md rule 3).
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
   rootfs is bind-mounted to a constant path. See ARCHITECTURE.md. Host-agnostic
   holds *within a CPU vendor pool*: the disk half is vendor-free, and a memory
   image is never restored across the Intel/AMD boundary (ARCHITECTURE.md
   rule 6), which is why a machine whose pool has no live host cold-boots from
   its own disk instead.
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
  print nothing), `go vet ./...` and `go test ./...`; the same three from
  `sdks/go/` and from `apps/pilot/`; from the root, `npm test`. These are exactly what CI runs --
  gofmt was missing from this list while CI enforced it, so a branch could be
  green locally and red on main. Note gofmt reformats DOC comments as well as
  code: it turns `''` and ``` `` ``` into curly quotes, which quietly mangles a
  comment that meant an empty SQL string. Also run `bash -n` on every shell
  script you edited and `node --check scripts/e2e.mjs` — a syntax error in
  either is only found at the moment it is needed, on a host, mid-bootstrap.
- **A new background loop registers a liveness budget.** One line at the top
  of the loop (`metrics.NewLoop(name, budget)`) and one `Tick()` at the END of
  each pass, budget normally three times the interval. `Restart=always` only
  catches a loop that dies; the failure that actually happens is a loop that
  wedges while the process stays healthy, and the watchdog withholds its pet
  on an overdue budget so systemd restarts the host. A loop with no budget is
  invisible to that, which is the whole of the bug.
- Every phase issue (#2–#7) carries a **gate checklist**. An issue closes
  when its gate is green, not when the code is written.

### Testing

- `scripts/e2e.mjs` is the single, **monotonically growing** battery. Later
  phases add assertions; they never retire earlier ones.
- It drives the **public API only** — the same surface an SDK or agent uses.
  A green run therefore exercises routing + hostd + Firecracker together.
- It skips cleanly (exit 0) unless `PILOTS_E2E=1`, so `npm test` stays green
  on machines with no KVM.
- It drives the exec stream with Node's **global `WebSocket`**, so it needs
  Node 22 or newer. The root `package.json` already requires 24, so this costs
  nothing; it is written down because the failure otherwise reads as a broken
  route rather than a runtime that has no `WebSocket`.
- Its API key comes from `hostd bootstrap-key` and must carry `admin`: the
  battery drives routes from every scope, and a narrower key turns real
  assertions into 403s.
- The **agent gate** inside it takes a webjs app with no Dockerfile and no
  compose file to a 200 on `/__webjs/ready` through the router in ONE `deploy`
  call, and drives the `unknown` loop: a directory nothing recognises answers
  with what an agent needs to write a Dockerfile, and the run writes one. That
  is bar 4 as a test rather than a claim.
- `scripts/cluster/gate.sh` is the fleet battery, a numbered `say` section per
  property, run against the local multi-node rig. It grows monotonically too.
  Section 43 covers Postgres high availability: the nodes on distinct hosts,
  a leader killed with -9 the way a host death kills one, and the seconds from
  that kill to a successful WRITE through the unchanged address -- not "a node
  was promoted" but "an application can write again".
  Section 42 covers per-machine numbers and log rotation: the reported memory
  against what the kernel itself says, a scrape on a host that does NOT own the
  machine, and a console log that stays bounded with its writer still appending
  at the new end after a rotation.
  Section 41 covers the credential broker: a socket bound inside one machine's
  namespace and in no other, nothing holding a token in the clear on the host,
  and a token minted on one host accepted by another with no lookup -- none of
  which is observable from the public API alone.
  Section 22 covers the push path against `scripts/cluster/fake-github.py`,
  because a delivery has no client on the other end and nothing about it is
  observable from the public API alone. Section 23 kills a build's client
  mid-build and reads the release it still cut from a host that did not build
  it — the shell half of the e2e battery's `deployOnVerdictAssertions`.
  Section 25 asserts no host runs a build daemon at all, and kills a builder
  machine mid-build: a build runs inside a microVM, so the wreckage of one
  belongs to that guest and nothing of it may survive on the host.
  Sections 28 and 29 are the join gate and its negative control: a host whose
  peers it cannot reach reports itself as still joining, keeps serving its own
  machines, and claims none; with the gate skipped behind two fault flags, the
  same host declares itself caught up, which is the judgement section 28
  refuses to make.

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

The hard-won lessons from solving these Firecracker problems before are
already recorded in ARCHITECTURE.md, which is why that file is the source of
truth and why it says to change it BEFORE changing code that contradicts it.
Read it first; the reasoning behind an invariant is usually an incident
somebody already paid for.

Two components are explicitly **ports, not rewrites** — they encode kernel
ABI details that are expensive to rediscover:

- the **uffd handler** (`apps/hostd/internal/uffd`, run as the hostd
  subcommand `uffd-handler`)
- the **NBD handler** (`apps/hostd/internal/nbd`, run as the hostd subcommand
  `nbd-handler`)

### Where the hard problems are already solved

Two components here are explicit **ports, not rewrites**, because they encode
kernel ABI details that are expensive to rediscover: the uffd handler and the
NBD handler above.

For everything else in the engine -- microVM lifecycle, userfaultfd, block
chunking, netns and addressing, guest image fixups, how to MEASURE a snapshot
-- there is a complete open-source implementation of this product's hard half,
and the rule is to READ IT BEFORE GUESSING rather than after two rounds of
trial and error. That ordering is a rule because the alternative has already
cost time here.

The pointers, the file-by-file index, and what to take from it (mechanics,
never architecture) are in the prior-art repo below, along with the numbers
worth targeting. They are kept out of this file because they are specific
about named projects.

### Prior art: `docs/prior-art/` — read the relevant note before designing

> **It is a separate PRIVATE repo, cloned into this path.** The notes judge
> named projects by name, with explicit "what pilots should reject and why"
> sections. That is the right way to write them for an implementer and the
> wrong thing to publish at the projects being judged, so they are not
> vendored here. Clone once and every path below resolves:
>
> ```
> git clone git@github.com:vivek7405/pilots-prior-art.git docs/prior-art
> ```
>
> Without it the references below simply are not there. Nothing in the build
> or the tests depends on them; they are for whoever is designing.

Every design decision starts from what the established platforms did and why.
The notes are deep and sourced (URL or `path:line` on every claim; inference
marked). Start at `docs/prior-art/README.md` — it has the capability scorecard,
the routing table telling you which note answers which question, and the
file-by-file index into the open-source implementations worth reading before
you guess.

The COPY / REJECT / parity sections at the end of each note are the actionable
part; an issue that contradicts a REJECT entry must say why.
