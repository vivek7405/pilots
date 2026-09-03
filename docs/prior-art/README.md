# Prior art — what pilots measures itself against

_Research date: 2026-09-03. Five notes, each deep and sourced (URL or
`path:line` on every claim; "(inferred)" marked). This page is the synthesis
and the index. The bar, from the product definition: **at par with fly.io,
sprites.dev and e2b-infra, or better — resilient, robust, simple to maintain,
and extremely fast to deploy web apps, webjs apps above all.**_

| Note | What it is | Why it matters to pilots |
|---|---|---|
| [`fly-io.md`](./fly-io.md) | Fly Machines, flyd/flaps, Corrosion, Fly Proxy, 6PN, volumes, deploys, the 2024 outage post-mortems | The orchestration shape pilots copied (rule 7) and the operator of Corrosion at scale — their incidents are pilots' incident list in advance |
| [`sprites-dev.md`](./sprites-dev.md) | Fly's agent-sandbox product: lifecycle, disk-only checkpoints, warm/cold tiers, exec frames, env contract, pricing | crisp runs there today; the sandbox surface pilots must match byte-for-byte |
| [`e2b-infra.md`](./e2b-infra.md) | The open-source orchestrator: uffd, memfile diff chain, block store, templates, envd, netns, host provisioning | The kernel-facing mechanics (`#22` levers) with verified `path:line`; the central control plane pilots replaces, itemised |
| [`uncloud.md`](./uncloud.md) | Corrosion-based, no-control-plane Docker orchestrator | Where pilots' state model was borrowed from — and the correctness gaps its author left open that pilots must not inherit |
| [`webjs-deploy-contract.md`](./webjs-deploy-contract.md) | What a webjs app needs from a host | The primary workload; the deploy path is `npm install` + restore + `/__webjs/ready` |

## Scorecard

Capabilities from `ARCHITECTURE.md`'s table. "✓" = has it; "~" = partial or
weaker; "✗" = absent. pilots column = design + what `main` has on 2026-09-03.
Sources: each note's parity section.

| Capability | Fly Machines | sprites.dev | e2b | uncloud | pilots |
|---|---|---|---|---|---|
| No central control plane in any request path | ✗ Rails/Postgres API, tkdb, flaps | ✗ Elixir orchestrator + per-org SQLite | ✗ API/Postgres/Redis/Nomad/LaunchDarkly | ✓ Corrosion only | ✓ rule 1 |
| Processes per host | 8+ (flyd, proxy, corrosion, corro-dns, NATS, containerd, …) | n/a | orchestrator + envd + Nomad/Consul agents | uncloudd + corrosion + Docker + Caddy | 3 (hostd, corrosion, firecracker) |
| Instant create (from template) | ~ ~300ms start; create "over a minute" | ✓ ~1–2s (pooled) | ✓ restore, not boot | ✗ Docker pull | ✓ <1.5s target; 462ms measured (no reflink) |
| Named, chained, durable checkpoints | ✗ (suspend is single-use) | ✓ disk-only, ~300ms create; memory best-effort | ~ pause/resume; `Checkpoint` RPC not productised | ✗ | ✓ disk + memory in S3, async durable |
| In-place restore, same URL/token | ✗ | ✓ | ~ | ✗ | ✓ |
| Suspend/wake as a held request | ✓ (window undocumented) | ✓ 10s window; 100–500ms warm, 1–2s cold | ~ L7 via API | ✗ | ✓ <1s target; 94ms measured; **L3 wake** (unique) |
| Cross-host recreate from object storage | ✗ host-pinned volumes | ✓ "durable state is a URL" | ✗ sandbox dies with node | ✗ | ✓ rule 4 |
| Self-heal on host death, zero human action | ✗ "resilience is your problem" | ✓ claimed, mechanism unpublished | ✗ | ✗ "by design" | ✓ deterministic-owner rescue |
| Deploy any Dockerfile → service | ✓ remote builders | ✗ | ✗ | ✓ unregistry push | ✓ BuildKit + fleet-wide S3 layer cache |
| Custom domains + auto certs | ✓ | ✗ | ✗ | ✓ Caddy (per-machine cert stores, #31) | ✓ one wildcard via S3 + HTTP-01 any host |
| Health-gated rollout, kept-old rollback | ✓ bluegreen/rolling | ✗ | ✗ | ~ per-container, no post-deploy rollback | ✓ |
| Promote sandbox → production, identity preserved | ✗ | ✗ ("run prod on Machines") | ✗ | ✗ | ✓ **unique** |
| N-replica LB + concurrency autostop/autostart | ✓ soft/hard limits | ✗ | ✗ | ~ Caddy round-robin | ✓ |
| Volumes surviving host death | ✗ | ✓ | ✗ | ✗ | ✓ JuiceFS on S3 |
| Guest-to-guest private network + name discovery | ✓ 6PN + `.internal` | ✗ | ✗ | ✓ WG + DNS | ✓ `.internal` + tenant filter |
| Multi-tenant isolation | ✓ FC + jailer | ✓ FC + inner container + DNS egress policy | ~ FC as root, no jailer; SNI firewall | ✗ single-tenant | ✓ jailer + cgroups v2 + nftables egress |
| Memory-page mechanics (hugepage uffd, O(dirty) pause, prefetch) | n/a | n/a | ✓ (partly fork-gated) | n/a | ~ **#22** |

Where pilots is ahead by construction: no control plane, three processes,
promote, L3 wake, S3-as-truth for *memory* images, global content-addressed
dedup (e2b dedups only against the parent). Where it is behind on `main`
today: the items in **Gaps** below.

## Cross-cutting lessons (what more than one system says)

1. **The per-host daemon owns its machines; anything cross-host is a proposal.**
   Fly: flyd is "the source of truth" for its VMs and flaps only asks; e2b's
   orchestrator likewise never consults the API for anything on the data path;
   uncloud's daemon writes only its own rows. pilots rule 7 is this. Two
   pieces Fly adds that pilots lacks: **best-fit (not bin-pack) ranking over
   live capacity** and **template-cache presence as a placement input**
   (`fly-io.md` COPY 3–4).
2. **Make every lifecycle op a journaled FSM so a daemon restart resumes it.**
   Fly deploys flyd daily because of this; e2b instead kills sandboxes on
   orchestrator restart (a REJECT). pilots' hostd should re-adopt live FC
   processes and resume in-flight ops from a local journal (`fly-io.md` COPY 1;
   `e2b-infra.md` REJECT "kill-on-restart").
3. **Corrosion is safe only with discipline the schema cannot enforce.**
   Both operators converge on: write whole rows, never deltas; additive-only
   schema with defaults, rolled out before consumers; never write inside a
   retry loop; cap gossip queues; watchdog every subscriber loop; snapshot the
   DB to S3 and script a re-seed. uncloud adds the piece Fly's posts do not
   spell out: **gate every store-dependent component on the crsql version
   vector at join and on an empty `__corro_bookkeeping_gaps`** — a half-
   replicated `hosts` table would make pilots' self-heal rescue live machines
   (`fly-io.md` COPY 5–9; `uncloud.md` COPY 1–4, 8).
4. **Never encode a host into anything durable.** Fly's 6PN addresses and
   host-pinned volumes cost three years of migration work; sprites fixed it
   with "durable state is a URL". pilots rules 4–5 already forbid it; the
   research confirms it is the single most expensive mistake to reverse.
5. **Checkpoint = metadata copy over immutable chunks; memory is a fast path,
   disk is the durable tier.** sprites says it outright; e2b's diff chain is
   the same shape one level down; Fly's suspend (full dump, ≤2GB, single-use)
   is the anti-pattern. pilots keeps memory images durable in S3 (better than
   all three) but must keep "restore never fails because the memory image is
   gone" as a stated API contract (`sprites-dev.md` COPY 1–2).
6. **Wake-on-request is held at the proxy with a published window.** sprites
   10s, Fly undocumented, e2b via the API. pilots' L3 wake is the only one that
   holds raw TCP for `.internal` peers; publish its window too.
7. **Isolate auth from the data path.** Fly's tkdb outage and Sep-2024 proxy
   incident both got worse because token validation crossed the network.
   pilots validates API keys and agent tokens from local state only — keep
   the dashboard host out of every request path (`fly-io.md` COPY 18).
8. **The create path must never pull a per-tenant image.** "Most of what's
   slow about creating a Fly Machine is containers." e2b and sprites both
   restore pooled/templated VMs. pilots builds once to content-addressed
   chunks and lazy-faults — and for webjs, a redeploy is one layer.

## Gaps this research surfaced on `main` (2026-09-03)

Concrete, each pointing at the note that specifies the fix. File issues from
here; do not fix blind.

| Gap | Where it bites | Note |
|---|---|---|
| Rootfs user is `user` (`scripts/rootfs/Dockerfile:43`); no `/home/sprite`, no nvm shim | crisp drop-in (#7 gate) hardcodes `/home/sprite/app` | `sprites-dev.md` §6 |
| Exec frame protocol has ids 1/2/3 only; missing 0 (stdin), 4 (stdin_eof), JSON `resize`/`exit`/`port_opened`, `/control` multiplex | the frame half only — SDK parity and the crisp adapter are closed by `@pilots/sdk/sprites-compat` | `sprites-dev.md` §5, COPY 5 |
| No join gate: self-heal/DNS/mesh can run on a half-replicated replica | rescue of live machines after a host reboot | `uncloud.md` COPY 1; open Q1 |
| Placement is ownership-hash only; no best-fit ranking, no template-cache affinity | create latency + "Katamari" imbalance on a multi-host fleet | `fly-io.md` COPY 2–4 |
| No lifecycle journal / re-adoption on hostd restart | in-flight checkpoint lost on daemon upgrade | `fly-io.md` COPY 1 |
| Router/DNS must join on `hosts.last_seen` (membership-aware), and prefer the local replica | dead host keeps 1/N of traffic; cross-mesh hops when a local replica exists | `uncloud.md` COPY 7, REJECT 3 |
| Host-bootstrap lacks e2b's sysctls, hugepage math, NBD `nowatch` udev rule (#22), ~~public-reachability self-check~~ (landed) | #22 gate 5; a firewalled host silently eats traffic | `e2b-infra.md` COPY 13, 19–20; `uncloud.md` COPY 13 |
| ~~No `docs/incidents/` log~~ (landed: `docs/incidents/`), no `/v1/hosts` replication-lag field (#30) | first self-heal misfire will be undebuggable | `fly-io.md` COPY 20; `uncloud.md` COPY 11 |
| Rollout probe default should be `/__webjs/ready` (200 only) for webjs apps; router must set `X-Forwarded-Proto` and speak h2 to browsers | webjs HSTS never turns on; cold instance cut over; slow page loads | `webjs-deploy-contract.md` |
| Auto-checkpoints with tiered retention; last-N checkpoints mounted read-only in the guest; port-open notifications | agent DX parity with sprites | `sprites-dev.md` COPY 3–4, 6 |
| #22 levers (hugepage uffd, `Diff` snapshots, last-cycle prefetch, pre-pause reclaim, fs-only snapshots) | beyond-SLO speed | `e2b-infra.md` COPY 1–12, 23 |

## Rejected by name (do not relitigate without new evidence)

- Central API / Postgres / Redis catalog / Nomad / feature-flag service on any
  request path (all three commercial systems; `fly-io.md`, `e2b-infra.md`).
- Forking Firecracker or patching guest kernels (`e2b-infra.md` REJECT; #22).
- Read-then-write uniqueness on the replica; Redlock across hosts
  (`uncloud.md` REJECT 1, 6).
- Host-pinned volumes, host-encoded addresses, per-machine cert stores
  (`fly-io.md`, `uncloud.md`).
- Non-durable single-use suspend snapshots (`fly-io.md`).
- Org id or host id in a URL; private-by-default URLs for the PaaS face
  (`sprites-dev.md` REJECT 2–3).
- NBD across hosts ("stuck nbd kernel threads" → Fly moved to iSCSI); pilots'
  NBD is local-only (`fly-io.md`).
- L7-only wake; kill-on-restart; NFS/P2P chunk caches at pilots' scale
  (`e2b-infra.md` REJECT).

## Which note answers which question

| Question | Note § |
|---|---|
| How should placement pick a host? | `fly-io.md` §2, COPY 2–4 |
| What breaks Corrosion in production? | `fly-io.md` §3, §10; `uncloud.md` "Shared state" |
| What must a host do before trusting its replica? | `uncloud.md` COPY 1–3 |
| How do suspend/wake tiers behave and what do they cost? | `sprites-dev.md` §3; `fly-io.md` §4 |
| What exactly is in a checkpoint? | `sprites-dev.md` §2; `e2b-infra.md` §3, §5 |
| How does the exec/WS protocol look on the wire? | `sprites-dev.md` §5 |
| What env does an agent sandbox expect? | `sprites-dev.md` §6; memory `crisp-on-pilots-env-contract` |
| uffd ioctl sequence for 2MiB pages, zero-fill, EAGAIN? | `e2b-infra.md` §3, COPY 1–5 |
| How is O(dirty) pause done? | `e2b-infra.md` §3, REJECT (fork note); #22 lever 2 |
| Netns / constant addressing / firewall details? | `e2b-infra.md` §6, COPY 14–15 |
| Guest agent (`/init`, clock, token) behaviour? | `e2b-infra.md` §7, COPY 16–17 |
| Host sysctls, hugepages, NBD udev? | `e2b-infra.md` COPY 13, 19–20 |
| How does the proxy load-balance and autostart? | `fly-io.md` §5, COPY 11–13 |
| How should certs be issued safely? | `fly-io.md` COPY 14; `uncloud.md` REJECT 5 |
| How do deploys get gated and rolled back? | `fly-io.md` COPY 15; `uncloud.md` §3 |
| What does a webjs app need to deploy fast? | `webjs-deploy-contract.md` |
| What has each competitor published as a number? | `fly-io.md` §11; `sprites-dev.md` §10; `e2b-infra.md` §11 (harness only) |

## Keeping this current

Re-run a track when its subject ships something material (Fly infra-log,
sprites changelog, e2b-infra `git pull`, uncloud releases). Each note's header
carries its research date and (for local clones) the commit it was read at;
diff from there. Add findings here only if they change the scorecard, a
cross-cutting lesson, or the gap list — detail belongs in the note.
