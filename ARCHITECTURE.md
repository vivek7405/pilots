# pilots — architecture

The design source of truth for this repo. Everything here is settled and
load-bearing: several invariants below were paid for with production
incidents in the predecessor codebase, and the contracts are what every
phase builds against in parallel.

**The phase plan and build order live in the issue tracker**, not here —
see [#1](https://github.com/vivek7405/pilots/issues/1) (master plan) and
its sub-issues #2–#7. Workflow rules live in [`AGENTS.md`](./AGENTS.md).

Change this file *before* changing code that contradicts it.

---

## Product definition (full parity — nothing deferred)

One primitive: the **machine** (a Firecracker microVM booting an ext4 rootfs,
with an identity that never changes). Lifecycle is per-machine config, not a
mode: `autoStop: off|stop|suspend`, `autoStart: bool`,
`minMachinesRunning: int` (0 = scale-to-zero, valid for production services).

| Capability | Definition of done |
|---|---|
| Instant create | from-template restore <1.5s |
| Exec | buffered + WS-streaming, `cwd`+`env`+`user`, stdin optional |
| Instant checkpoint | resume-gap <500ms; named; chained; durable async |
| In-place restore | same machine row, same URL, same agent token |
| Suspend/wake | idle-suspend (timer AND concurrency); wake <1s as a held request — never a waiting page |
| Cross-host | any machine recreates on any host from S3 alone |
| Self-heal | dead host's machines return on survivors, zero human action |
| Deploy | any Dockerfile → running service, streamed structured build logs |
| Custom domains | CNAME + automatic per-domain certs |
| Rollout | health-gated deploy, kept-old rollback |
| Promote | sandbox checkpoint → production service, identity preserved |
| N-replica | router LB, concurrency-driven autostop/autostart |
| Volumes | persistent, per-write durable, survive host death |
| Surface | CLI, JS/Go SDKs, MCP server, webjs dashboard |
| Multi-tenant | jailer + cgroups v2 + egress firewall + quotas |

Post-parity backlog (beyond every competitor): CoW memory fan-out (1→N
fork), tenant Postgres-as-a-service, multi-region.

---

## Architecture rules (each is load-bearing)

1. **No central control plane.** No request path may require a specific
   machine to be alive. Every host runs the identical stack and serves the
   full API. There is no scheduler tier, no managed DB, no LB appliance.
2. **Three processes per host:** `hostd` (Go, ours — the entire data plane),
   `corrosion` (Fly's gossip-replicated SQLite, run as a binary),
   `firecracker` (spawned per machine). Systemd manages hostd + corrosion.
3. **State = Corrosion.** cr-sqlite CRDT tables gossiped via SWIM/QUIC;
   every host reads its local replica (µs, no network). Last-write-wins ⇒
   no uniqueness constraints and no cross-host transactions, therefore:
   - **Single-writer invariant:** a host writes ONLY rows about its own
     machines. Enforced in review; violations corrupt silently.
   - **Deterministic ownership** for anything needing uniqueness or an
     actor: `hash(key) mod live_hosts` (name allocation, self-heal slices).
4. **S3 is the only truth for machine state.** Hetzner Object Storage
   (S3-compatible, path-style; internal eu-central traffic is free — compute
   must live in FSN1/NBG1). Local NVMe is strictly a cache; the design test
   is "wipe any host's disk; nothing is lost."
5. **URLs are permanent.** `<name>.pilotrun.app` for every workload (sandbox
   AND service — one apex, because promote must not change URLs);
   `<port>-<name>.pilotrun.app` for arbitrary ports; `pilots.run` for the
   dashboard ONLY (user code must never share the dashboard apex — a guest
   on the same apex could set cookies scoped to it). `pilotrun.app` is
   HSTS-preloaded → browsers force HTTPS on every subdomain (iframe-safe).
   Wildcard DNS `*.pilotrun.app` → A records for ALL host IPs (Cloudflare
   free tier). One wildcard cert via ACME DNS-01 (certmagic + Cloudflare API
   token), shared to hosts via S3. Custom domains: per-domain on-demand
   certs via HTTP-01 (any host can answer the challenge).
6. **Fleet is CPU-vendor-homogeneous — vendor is a cost decision, not a
   technical one.** FC memory snapshots carry raw CPUID; a snapshot never
   restores across the Intel/AMD boundary (cpu_templates normalize within a
   vendor, not across). Since this is a fresh build with no snapshot lineage,
   the fleet can be **all-Intel (Hetzner EX line / auction i7 — often
   cheaper)** just as well as all-AMD; pick whichever is cheapest at order
   time and pin the matching `cpu_template` (T2/T2CL Intel, T2A AMD) so
   later same-vendor host generations mix safely. Two consequences: the dev
   laptop is AMD, so laptop-built snapshots/templates never ship to an Intel
   fleet — golden templates are always built ON the fleet by CI; and auction
   i7 desktop boards have non-ECC RAM (acceptable at this stage; note it).
7. **Fly-shaped orchestration, sprites-shaped storage.** Per-host autonomy
   (each host acts on its own machines: wake, restart, suspend, health) +
   any-host coordination (any host serves any API request, proposing
   placements that target hosts may refuse — "coordinators propose, hosts
   dispose"). Storage is content-addressed and host-agnostic (better than
   Fly's host-pinned volumes).

---

## Contracts (fixed before parallel work begins)

### Corrosion schema (also the Phase-2/2 local SQLite schema, verbatim)

```sql
CREATE TABLE hosts    (id TEXT PRIMARY KEY, wg_addr TEXT, public_ip TEXT,
                       cpu_free INTEGER, mem_free_mib INTEGER,
                       last_seen INTEGER);           -- writer: the host itself
CREATE TABLE machines (id TEXT PRIMARY KEY, name TEXT, host_id TEXT,
                       state TEXT,   -- creating|running|suspended|stopped|error
                       kind_knobs TEXT,  -- json: auto_stop/auto_start/min_machines_running/soft_limit
                       image_ref TEXT, vcpus INTEGER, mem_mib INTEGER,
                       domain TEXT, custom_domain TEXT,
                       app_port INTEGER, agent_port INTEGER,
                       agent_token_hash TEXT,
                       mem_build_id TEXT, rootfs_build_id TEXT, -- latest snapshot
                       volume_id TEXT, service_id TEXT, release_id TEXT,
                       last_activity INTEGER, updated_at INTEGER);
                                                     -- writer: host_id only
CREATE TABLE checkpoints (id TEXT PRIMARY KEY, machine_id TEXT, seq INTEGER,
                       comment TEXT, source_id TEXT,
                       mem_build_id TEXT, rootfs_build_id TEXT,
                       durable INTEGER, created_at INTEGER);
CREATE TABLE api_keys (hash TEXT PRIMARY KEY, org_id TEXT, scopes TEXT,
                       created_at INTEGER);          -- writer: dashboard's host
CREATE TABLE releases (id TEXT PRIMARY KEY, service_id TEXT,
                       rootfs_build_id TEXT, healthy INTEGER,
                       created_at INTEGER);
```

### hostd HTTP API (public; every host serves it; bearer auth)

```
POST   /v1/machines                  create {name?, image|template|checkpoint, vcpus, mem, knobs, volume?}
GET    /v1/machines                  list
GET    /v1/machines/:id              info
DELETE /v1/machines/:id              destroy
POST   /v1/machines/:id/exec         {cmd, cwd?, env?, user?, timeout?} → {stdout, stderr, exitCode}
GET    /v1/machines/:id/exec/stream  WS: query argv/dir/env/stdin → frames (below)
GET    /v1/machines/:id/logs?follow  stream
POST   /v1/machines/:id/suspend|wake|stop|start
POST   /v1/machines/:id/checkpoints  {comment?} → {id, seq}
GET    /v1/machines/:id/checkpoints  list
POST   /v1/checkpoints/:id/restore   in-place restore
POST   /v1/builds                    {dockerfile-context tar} → streamed structured log → {rootfs_build_id}
POST   /v1/services                  {name, release|build, replicas, health, domain?}
GET    /v1/services                  list
GET    /v1/services/:id              info
POST   /v1/services/:id/deploy       health-gated cutover
POST   /v1/services/:id/rollback
POST   /v1/machines/:id/promote      {domain?} → service
POST   /v1/volumes                   create JuiceFS volume
GET    /v1/volumes                   list
GET    /v1/hosts                     fleet view
GET    /v1/health                    liveness (unauthenticated)
GET    /metrics                      Prometheus (unauthenticated)
```

### Guest-agent protocol (inside every VM, port 3001)

`GET /health` · `POST /init {timestamp_nanos}` (sets CLOCK_REALTIME — kvm-clock
covers MONOTONIC; without this poke a restored guest's TLS/cron/JS clocks are
frozen at snapshot time) · `POST /exec` (buffered; `bash -c`; default user
uid-1000, root opt-in) · `GET /exec/stream` WS — binary frames, **byte 0:
1=stdout 2=stderr 3=exit (payload[0]=code)**, single write-mutex; `stdin=false`
supported (the agent-runner path) · `GET /terminal` WS (pty, JSON frames) ·
reverse proxy: any request bearing `X-Pilot-Proxy-Port: <n>` is proxied to
`127.0.0.1:<n>` (WS included; not auth-gated — the edge enforces). Token at
`/etc/pilot-agent/token`, constant-time compare, header or `?token=`.

### S3 layout (all under prefix `chunks/`)

```
chunks/<build-uuid>/header        # snapshot index (format below)
chunks/<build-uuid>/data          # packed non-zero/divergent 4KiB blocks
chunks/<mem-build-uuid>/snap.bin  # FC vmstate
chunks/<mem-build-uuid>/prefetch.txt   # "<off> <len>\n" fault-order replay
volumes/...                       # JuiceFS-managed
certs/...                         # shared wildcard cert material
```

**Header format** (little-endian): 64-byte metadata
`{Version=3, BlockSize=4096, Size, Generation, BuildId, BaseBuildId}` +
N × 40-byte maps `{Offset, Length, BuildId, BuildStorageOffset}`. A build with
`Generation=0, BaseBuildId==BuildId` is a template; a diff is generation+1
pointing at its parent. A **nil BuildId mapping = zero-filled gap**. Diff
chains are exactly two levels (template → per-machine diff); a grandparent
reference is a hard error. Local mirror: `/var/cache/pilot-build/<uuid>/`.

### S3 client = 4 operations only

`GetObject`, `GetRange` (must surface HTTP 416), `PutObject`, `PutFile`
(streamed). Path-style addressing. Config via env
`PILOT_S3_{ENDPOINT,REGION,BUCKET,ACCESS_KEY,SECRET_KEY}`.

---

## Engine mechanics (the knowledge, inline)

**Boot:** FC spawned per machine inside its own netns, API over unix socket:
PUT `/machine-config` (vcpus, mem, `smt:false`) → `/boot-source` →
`/drives/rootfs` → `/network-interfaces/eth0` (tap `vmnet`, random
`02:xx…` MAC) → `/actions InstanceStart`. Boot args:
`console=ttyS0 reboot=k panic=1 pci=off ro root=/dev/vda clocksource=kvm-clock
random.trust_cpu=on i8042.nokbd i8042.noaux ipv6.disable=0 ipv6.autoconf=1
ip=169.254.0.21::169.254.0.22:255.255.255.252:instance:eth0:off:`.
Kernel: a pinned vmlinux 6.1.x built/downloaded once into
`/opt/pilots/kernels/`. Run under **jailer** with a cgroup v2 slice
(cpu.max, memory.max, pids.max) — non-negotiable for multi-tenant.

**The constant-IP netns slot model** (what makes snapshots host-agnostic):
every guest sees the SAME network — eth0 `169.254.0.21/30`, gateway (tap)
`169.254.0.22`. Slot-specific addressing (host-facing `10.11.0.<idx>/32`,
veth `/31` pair `10.12.0.<2i>/<2i+1>`) exists ONLY in the netns's
iptables SNAT/DNAT, which is rebuilt at restore — never inside the snapshot.
Slot pool of 1024/host. In-netns nft table drops guest egress to
RFC1918/loopback/link-local/ULA. All netns/tap/nft setup implemented in Go
(netlink), not shelled bash.

**The rootfs bind-mount trick** (a shared rootfs causes post-resume workqueue
lockups; the snapshot bakes an absolute drive path): FC runs under
`unshare -m`; a per-machine rootfs (reflink copy of the template, on NVMe —
not tmpfs) is bind-mounted onto the constant path
`/srv/pilots/rootfs.ext4` inside that private mount ns. Every snapshot
therefore restores against the same path on any host.

**Snapshot (suspend/checkpoint):** PATCH `/vm Paused` → PUT `/snapshot/create
{Full, snap.bin, mem.bin}`. Then:
- *Suspend*: chunkify mem.bin (diff vs template mem build) + rootfs cow →
  upload → delete local mem.bin → kill VM → upload prefetch capture.
- *Checkpoint (instant)*: `cp --reflink=auto --sparse=always` the cow →
  **resume immediately** → chunkify+upload in a bounded background worker
  (semaphore, default 1 — unbounded chunkify OOMs hosts) → durability
  signalled by `.durable`/`.failed` marker files, exposed via
  checkpoint-status (`durable/present/failed` — `present=false` means
  "cross-host: fetch from S3").
- Skip the rootfs build entirely when the cow has 0 allocated blocks.
- Flush with `SyncFileRange` on the cow fd — NEVER global `sync()`
  (holds the kernel bdev lock; concurrent suspends serialize for minutes).

**Restore (create-from-template, wake, checkpoint-restore — same path):**
prefetch snap.bin + prefetch.txt from S3 if absent → netns setup and
uffd-handler spawn run in parallel goroutines (saves 150–250ms), router
port-binding overlaps FC start → PUT `/snapshot/load {mem_backend: Uffd
socket | File}` with `resume_vm:false` → PATCH `/vm Resumed` → async POST
guest `/init {now}` (5ms retry loop, 15s deadline).

**Lazy memory (uffd handler):** receives the uffd fd via SCM_RIGHTS on FC's
socket + a JSON region map `{base_host_virt_addr, size, offset, page_size}`;
serves guest page faults with `UFFDIO_COPY` (x86_64 ioctls; EAGAIN retry,
EEXIST=success, short-copy detection); 4 fault workers. **Coalesced
prefault**: one bulk `GetRange` of the packed data file in a goroutine —
without it, cold restore = one ~50ms S3 round-trip per 4KiB fault (~70min for
256MiB). Records fault order to prefetch.txt for next-restore replay (read
the replay file fully BEFORE creating the record file — commonly same path).
Mixed page sizes (hugepages+uffd) unsupported. Port this handler from the old
code (`cmd/pilot-uffd-handler`, 881 LOC) — hand-rolled kernel ABI; do not
rewrite from scratch.

**Lazy disk (NBD handler):** kernel NBD split-mode via socketpair fd handoff
(28-byte BE requests / 16-byte BE replies); serves a block `Overlay` =
template (read-through) + per-machine cow `Cache` (sparse mmap'd file +
roaring bitmap of dirty 4KiB blocks; reads hit cache only if EVERY covered
block is dirty, else fall through). Rehydrate-on-wake populates the cache
from the machine's diff build BEFORE accepting requests, **skipping
parent-pointing mappings** (marking them dirty would shadow template content
with zeros). An all-zero diff yields a zero-length data object → S3 range GET
returns **416: treat as zeros, mark cached** (else wake dies on an NBD sizing
timeout). If the local data file is already ≥ packed size, mark all cached
up-front (else restores crawl at 10–20 faults/s). Pick free NBD devices via
`/sys/block/nbdN/pid` (a `blockdev` probe hangs on half-attached devices).
Issue `NBD_DISCONNECT` ioctl BEFORE killing a handler — otherwise FC blocks
in D-state and `/dev/nbdN` is dead until host reboot.

**Process management:** every child (fc, uffd, nbd) spawned under a
background context (never an HTTP request ctx), `Setpgid`, killed as
SIGTERM → Wait(2s) → SIGKILL, in dependency order, with NBD_DISCONNECT
first. FC's API accepts ~10 connections total → `DisableKeepAlives` on every
FC API call. netns delete returns EBUSY while a just-killed FC zombie holds
the ns fd → retry ~10× with 300ms sleeps; create is idempotent (destroy
first). **Persist + reconcile:** after every start/restore write `fc.pid` +
atomic `state.json` per machine; on hostd restart re-adopt via
`/proc/<pid>/comm == "firecracker"` (guards PID recycling), re-reserve slots
and ports WITHOUT bind-probing (the live proxy holds them). Bounded shutdown
(≤30s, then leave stragglers to the reaper). A reaper loop kills FC processes
with no matching machine row (60s age guard). Snapshot a fresh boot only
after the guest reaches `system-running` (~20s settle) — earlier captures a
half-converged guest that can't serve after resume.

**Router (in hostd):** TLS termination (wildcard + on-demand custom-domain
certs) → hostname parse (`name` | `port-name` | custom domain) → local
Corrosion lookup → if running-local: proxy into the netns (in-process Go
proxy — no socat, no per-VM forwarder processes); if running-remote: proxy
over WireGuard to the owning hostd; if suspended and `autoStart`: **hold the
connection**, restore locally (or trigger the owner), then proxy. Touch
`last_activity` on every request AND every exec. Idle monitor suspends when
BOTH the wall-clock timer (default 60s, per-machine) and concurrency
(in-flight = 0 against `softLimit`) say idle — exec/WS activity counts, so an
agent mid-build with zero HTTP traffic is never suspended. N-replica:
round-robin among healthy replicas, `softLimit` overflow starts the next
stopped replica, excess capacity stops them (respecting
`minMachinesRunning`).

**Self-heal:** every hostd heartbeats `hosts.last_seen`; a host silent
>30s is dead; each survivor rescues the slice
`hash(machine_id) mod live_hosts == my_index` — recreate from the machine's
latest builds in S3, write the new `host_id`, URL unchanged. No leader, no
election. Placement double-booking is prevented by hosts being final
authority on their own capacity (a create/rescue targeting a full host is
refused and re-hashed).

**Build path:** `POST /v1/builds` accepts a Dockerfile context → BuildKit
(rootless buildkitd on each host) → image → flatten (`ctr`/`skopeo` export
or `docker export` equivalent) → `mke2fs -d` into an ext4 → chunkify as a
generation-0 template build → S3. Structured NDJSON log stream
(`{step, stream, line, ts}`) so an agent can parse failures and loop.

**Prompt-to-URL deploy (the AI-agent front door):** the headline UX is
"deploy this app" pointed at ANY codebase (Django, Rails, Next.js, Remix,
webjs, …) → a running `<name>.pilotrun.app`. Resolution order, all
agent-drivable via MCP:
1. Repo has a Dockerfile → build it.
2. No Dockerfile → **the driving agent generates one** (the pilots MCP
   server ships a `generate_dockerfile` prompt/tool with per-framework
   recipes: detect via lockfiles/manage.py/Gemfile/next.config/etc., pick
   base image, ports, start command).
3. Build fails → the agent reads the structured NDJSON error, patches the
   Dockerfile, retries — the loop the structured logs exist for.
**Deploy ingestion — two paths, one pipeline:**
1. **Direct** (base primitive, GitHub-free): `pilot deploy` tars the local
   context → `POST /v1/builds` → health-gated deploy. What the CLI, SDKs,
   and the agent flow all use.
2. **GitHub-connected (pilots GitHub App):** connect a repo once →
   push-to-deploy. The webhook endpoint is just another hostd route behind
   the wildcard DNS — any host receives it, `hash(repo) mod live_hosts`
   picks the builder; no central CI service. Config on the service row:
   repo, branch, autodeploy on/off.
3. **PR previews (free differentiator):** PR opened → build → a *sandbox*
   at `pr-<n>-<app>.pilotrun.app`, idle-suspended (≈zero cost), destroyed on
   merge/close; status posted back to the PR via the App.
The agent flow composes: after a prompt-to-URL deploy the agent offers to
connect the repo for continuous deploys.

The MCP toolset makes the whole journey agent-executable end-to-end:
`create_machine, exec, checkpoint, restore, build, deploy, promote, logs,
status` — so "deploy this app on pilots" is one agent conversation with a
URL at the end. This flow IS the product demo and gets its own e2e: point
an agent at a bare Django app with no Dockerfile → live URL, no human.

**Volumes:** JuiceFS mounted into the guest via virtio-fs or a second
virtio-blk; chunks on the same S3 bucket; metadata engine is a per-host
local Redis/SQLite kept durable via Litestream→S3 (no central metadata
service). Volume machines pin to hosts only while mounted; on host death the
volume remounts wherever the machine is rescued (per-write durability — this
beats checkpoint-granularity disk state and is where app data belongs).

---

## Authentication (first-class GitHub login)

- **Humans:** dashboard sign-in = **Login with GitHub** (webjs
  `createAuth({ providers: [GitHub] })`; handlers at
  `app/api/auth/[...path]`; `AUTH_SECRET` env). No passwords stored, ever.
- **CLI:** `pilot login` runs the **GitHub device flow** (prints a code +
  URL, polls) → the dashboard's API exchanges the GitHub identity for a
  pilots **API key** → stored at `~/.config/pilots/credentials`. Headless
  fallback: `pilot login --token` / `PILOT_API_KEY` env.
- **Machine auth:** every hostd request carries `Authorization: Bearer
  <api-key>`. Key **hashes** live in the Corrosion `api_keys` table
  (writer: the dashboard's host), so **every host authenticates locally**
  with no auth-service round-trip — auth survives any host loss, including
  the dashboard's.
- **Agents are first-class principals:** an API key is all an agent needs;
  scopes on the key (`machines`, `deploy`, `admin`) bound what it can do.
  The MCP server reads the same credentials file/env.
- Per-machine **agent tokens** (guest exec auth) are minted at create,
  hashed into the machine row, never reused across machines.

---

## Crisp compatibility (the reference customer)

Non-negotiables, honored by the contracts above: stable per-machine URL
across every lifecycle event · multiple named checkpoints with **in-place**
restore (same row/URL/token — never respawn-from-template) · WS streaming
exec with `stdin=false` for `claude -p … --output-format stream-json` ·
`cwd` + `env` on every exec (buffered and streaming). The sprites byte frame
protocol (1/2/3) is kept exactly so crisp's client code drops in.

---

## Monorepo layout (webjs conventions honored)

```
pilots/
  AGENTS.md  CLAUDE.md(@AGENTS.md)  ARCHITECTURE.md   # this plan's content
  package.json            # npm workspaces: apps/dashboard, packages/cli, sdks/js
  apps/
    hostd/                # Go, own go.mod — the entire data plane
      cmd/hostd/  cmd/guest-agent/  cmd/chunkify/
      internal/{fc,block,uffd,nbd,netns,router,state,s3,build,volumes,selfheal}/
      systemd/            # hostd.service, corrosion.service
    dashboard/            # webjs full-stack app (scaffolded `npm create webjs`)
  packages/cli/           # `pilot` CLI (TS)
  sdks/js/  sdks/go/      # thin typed clients over hostd's API
  scripts/                # bash one-shots ONLY: host-bootstrap.sh <ip>,
                          #   build-golden-rootfs.sh, dev-vm.sh, e2e.mjs
```

webjs facts to honor: app joins root `workspaces`; delete the scaffold's
nested `.git`; commands run inside the app dir
(`npm run dev --workspace=apps/dashboard`); Node ≥24; gitignore
`**/.webjs/*` + `!**/.webjs/vendor/` (commit the vendor importmap);
`#*` subpath imports resolve per-member; SQLite via built-in `node:sqlite` +
Drizzle (pinned 1.0.0-rc.3) is in the box; API routes =
`app/<seg>/route.ts` with GET/POST/… + `WS` export; server actions =
`.server.ts` + `'use server'`; auth via `createAuth({providers})` +
`session()`; readiness at `/__webjs/ready` (the deploy health gate);
scaffold Dockerfile is buildless (`node:24-alpine`, `npm start`).
Dashboard owns orgs/users/API keys in its own Drizzle DB and pushes API-key
hashes into Corrosion (via its local hostd) so every host authenticates
locally; it is deployed ON the platform via promote and is never in the
request path.

---

## Verification (continuous)

`scripts/e2e.mjs` is the single growing battery: correctness from Phase 2,
timing from Phase 3, chaos from Phase 4, PaaS from Phase 5, hostility from
Phase 6 — later phases never retire earlier assertions. `go test ./...` for
netns/block/header/state/s3 (block-layer round-trip + diff-chain tests are
mandatory). Dashboard: `webjs check` / `doctor --json` / `typecheck` /
`test`. CI runs unit tests + the single-VM e2e on every push.
