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
`minMachinesRunning: int` (0 = scale-to-zero, the default on both faces; a
deploy sets it on its replicas, and a promoted sandbox keeps the knobs it had).

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
| Rollout | health-gated deploy, kept-old rollback; a volume-backed service is one machine redeployed in place |
| Promote | sandbox checkpoint → production service, identity preserved |
| N-replica | router LB, concurrency-driven autostop/autostart |
| Volumes | persistent, per-write durable, survive host death |
| Surface | CLI, JS/Go SDKs, MCP server, webjs dashboard |
| Multi-tenant | jailer + cgroups v2 + egress firewall + quotas |

Post-parity backlog (beyond every competitor): CoW memory fan-out (1→N
fork), multi-region. (Tenant Postgres left this list: it is a shipped
compose fragment on the ordinary primitives, not a product tier. See
**Databases** below.)

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
   The router owns the forwarded headers, set once at the public entry and
   preserved across a mesh hop: `X-Forwarded-For` is deleted inbound so what
   reaches the guest is the peer this edge saw, `X-Forwarded-Proto` is `https`
   when the edge terminated TLS, and `X-Forwarded-Host` is the name the user
   typed. A rate limiter behind us reads the leftmost entry, so a caller must
   not be able to supply it. The sibling client-IP headers (`X-Real-IP`,
   `CF-Connecting-IP`, `True-Client-IP` and RFC 7239 `Forwarded`) are deleted
   with it, since deleting only one of them moves the forgery rather than
   ending it.
   In code: every host calls `certmagic.ManageAsync` for `*.<workload
   domain>`, the workload apex and the dashboard apex; the shared `certs`
   Storage lock is what makes N hosts running the identical call produce ONE
   order, with the rest loading the result. Without a Cloudflare token the
   router stays HTTP-01-only and the wildcard is simply absent — the
   on-demand path still serves custom domains, so this degrades rather than
   fails. The API hostname `api.<workload domain>` needs no record and no
   certificate of its own: the wildcard A record and the wildcard
   certificate already cover it. `dispatch` claims that hostname for the
   control API before the workload suffix check, so every host answers the
   documented base URL. The machine name under that hostname is therefore
   reserved -- `api` by default: a tenant that took it would own a URL it
   could never be reached at. The reservation is derived from
   `PILOT_API_HOSTNAME`, so moving the control API moves it. It only guards
   creates, though, so a machine that already held the name when the API was
   pointed at it is reported rather than silently swallowed: hostd logs an
   error naming it and publishes `pilots_api_hostname_shadowed`. Refusing to
   start would turn one machine's lost URL into a fleet-wide outage, since
   the row is replicated to every host.
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
   The two halves of a template are built in different places and that split
   is the enforcement: the **rootfs** is an ext4 disk image with no CPUID in
   it, so CI builds it at a tag and `scripts/rootfs/golden.ext4.sha256` pins
   it — CI asserts the build matches the pin, and `host-bootstrap.sh`
   refuses to ship a local `golden.ext4` that does not. The **memory
   template** is never a file and is never shipped: it exists only as builds
   a fleet host chunkified from its own boot and published through its own
   replica, so no laptop can mint one. `PILOT_CPU_TEMPLATE` is pinned in
   `/etc/pilots/config` and the bootstrap refuses a host whose CPU vendor
   disagrees with it.
7. **Fly-shaped orchestration, sprites-shaped storage.** Per-host autonomy
   (each host acts on its own machines: wake, restart, suspend, health) +
   any-host coordination (any host serves any API request, proposing
   placements that target hosts may refuse — "coordinators propose, hosts
   dispose"). Storage is content-addressed and host-agnostic (better than
   Fly's host-pinned volumes).

8. **Guest page size is fleet-wide.** Guest memory is backed by 2MiB
   hugepages when the host is configured for it (`PILOT_HUGEPAGES`, with the
   pool reserved at boot by `host-bootstrap.sh`). The size is recorded IN
   every snapshot and cannot be reinterpreted at restore, so a host that
   disagrees with the fleet cannot restore the fleet's machines at all — not
   slowly, not at all. The template manifest records what it was photographed
   at, and a mismatch rebuilds rather than repairs. Capacity is then counted
   from `HugePages_Free`, not `MemAvailable`, which excludes the reserved
   pool: a host counting the wrong one advertises itself full and refuses
   every rescue. Swap must be off, because a swapped-out page is not resident
   and `mincore` would leave it out of the diff.

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
                       app TEXT,     -- grouping only; there is no apps table
                       last_activity INTEGER, updated_at INTEGER);
                                                     -- writer: host_id only
CREATE TABLE checkpoints (id TEXT PRIMARY KEY, machine_id TEXT, seq INTEGER,
                       comment TEXT, source_id TEXT,
                       mem_build_id TEXT, rootfs_build_id TEXT,
                       durable INTEGER, created_at INTEGER);
CREATE TABLE api_keys (hash TEXT PRIMARY KEY, org_id TEXT, scopes TEXT,
                       created_at INTEGER);
                       -- writer: any host, on an admin-scoped request
                       -- (write-once rows)
CREATE TABLE releases (id TEXT PRIMARY KEY, service_id TEXT,
                       rootfs_build_id TEXT, healthy INTEGER,
                       created_at INTEGER);
CREATE TABLE services  (id TEXT PRIMARY KEY, name TEXT, app TEXT,
                       release_id TEXT, replicas INTEGER,
                       health TEXT,      -- json, tagged union (below)
                       env TEXT,         -- json, non-secret only
                       env_sealed TEXT,  -- sealed blob; never plaintext
                       domain TEXT, custom_domain TEXT,
                       repo TEXT, branch TEXT, autodeploy INTEGER,
                       created_at INTEGER);           -- writer: host_id only

-- The volume a service mounts. A NEW table rather than a column on services,
-- for the reason domains and tenancy are ones: services carries rows, and
-- cr-sqlite backfills every row of a table whose columns change. The key is
-- <service_id>/<ordinal>, ordinal 1 today; a later volume-per-replica shape is
-- more rows here, never a column add.
CREATE TABLE service_volumes (id TEXT PRIMARY KEY, service_id TEXT,
                       ordinal INTEGER,  -- 1 today; see above
                       volume_id TEXT, created_at INTEGER);
                       -- writer: the service's arbiter (write-once)

-- Which org owns an object. A NEW table rather than an org_id column on
-- machines, services and volumes: those tables carry rows, and cr-sqlite
-- backfills every row of a table whose columns change -- the fleet-wide
-- gossip storm that took fly's fleet down twice for ~11.5h
-- (fly.io/infra-log/2024-11-30). A new table backfills nothing.
--
-- The row is written BEFORE the object row it names and is never changed, so
-- ANY host may write it: two writers cannot disagree about a value written
-- once, which is what makes "any host" legal under last-write-wins.
CREATE TABLE tenancy (id TEXT PRIMARY KEY, org_id TEXT,
                       kind TEXT,        -- machine|service|volume
                       created_at INTEGER);
                       -- writer: the host writing the object row (write-once)

-- A revoked key. A tombstone that only ever APPEARS: deleting the api_keys row
-- would be undone by a replica still carrying the older insert, and the key
-- would come back alive. Revocation therefore adds a row instead.
CREATE TABLE api_key_revocations (hash TEXT PRIMARY KEY, revoked_at INTEGER);
                       -- writer: any host, on an admin-scoped request
                       -- (write-once)

-- Per-org limits. One logical writer -- an admin request -- so last-write-wins
-- between two admins editing the same org is the intended semantics.
CREATE TABLE org_quotas (org_id TEXT PRIMARY KEY, max_machines INTEGER,
                       max_vcpus INTEGER, max_mem_mib INTEGER,
                       max_volume_gib INTEGER, max_builds INTEGER,
                       updated_at INTEGER);
                       -- writer: any host, on an admin-scoped request

-- Operator note for a fleet that is already bootstrapped: corrosion reads
-- schema_paths at agent start, so a host that has run before needs the new
-- schema.sql copied and its corrosion unit restarted before it can serve the
-- four tables above. They backfill nothing -- they have no rows -- so the
-- restart is the whole of the rollout.

-- Grouping is a property of the client's compose file, not a fleet object, so
-- there is deliberately no apps table. App names take their uniqueness from
-- the same hash(name) mod live_hosts owner that allocates machine names.
--
-- Health is a tagged union, because a database image ships a command check and
-- not an HTTP one:
--   {"type":"http","path":…,"interval":…,"timeout":…,"grace":…,
--    "healthy_threshold":…}
--   {"type":"cmd","test":["CMD-SHELL","pg_isready -U postgres"],
--    "interval":…,"timeout":…,"grace":…,"retries":…}
-- Docker semantics, so every stock image's own HEALTHCHECK maps straight in.
-- A service with no domain still health-gates and still rolls back, but it has
-- no concurrency signal -- so minMachinesRunning: 0 has no wake-on-request to
-- fall back on and is REJECTED at validation rather than silently redefined.
```

### hostd HTTP API (public; every host serves it; bearer auth)

```
POST   /v1/machines                  create {name?, image|template|checkpoint, vcpus, mem, knobs, volume?}
GET    /v1/machines                  list
GET    /v1/machines/:id              info
DELETE /v1/machines/:id              destroy
POST   /v1/machines/:id/exec         {cmd, cwd?, env?, user?, timeout?} → {stdout, stderr, exitCode}
GET    /v1/machines/:id/exec/stream  WS: query argv/dir/env/stdin → frames (below)
                                     auth: Authorization: Bearer, or the
                                     subprotocol authorization.bearer.<key> (browsers)
GET    /v1/sprites/:name/exec       WS alias of exec/stream, keyed by machine NAME
                                     (an id-shaped value is tried as an id first);
                                     sprites-compatible
GET    /v1/machines/:id/logs?follow  stream; a follow ends on disconnect, destroy,
                                     or a read that keeps failing (it says so on
                                     the stream), never on suspend
POST   /v1/machines/:id/suspend|wake|stop|start
POST   /v1/machines/:id/redeploy     {image, release?}  boot the same machine
                                     from another image, in place (the rollout's;
                                     a peer call carries the fleet's peer token)
POST   /v1/machines/:id/checkpoints  {comment?} → {id, seq}
GET    /v1/machines/:id/checkpoints  list
POST   /v1/checkpoints/:id/restore   in-place restore
POST   /v1/builds                    {dockerfile-context tar} → streamed structured log → {rootfs_build_id}
POST   /v1/services                  {name, release|build, replicas, health, domain?,
                                     volume?}; volume is create-only and pins
                                     replicas to one
GET    /v1/services                  list
GET    /v1/services/:id              info
PATCH  /v1/services/:id              {replicas?, health?, env?, secret_env?, repo?,
                                     branch?, autodeploy?}; env and secret_env
                                     REPLACE the stored map, and env, secret_env
                                     and replicas take effect at the NEXT deploy;
                                     knobs are refused with a 400 naming the field
                                     (they travel on the deploy); forwarded to the
                                     service's arbiter
GET    /v1/services/:id/releases     newest first, [] for none
POST   /v1/services/:id/deploy       health-gated cutover
POST   /v1/services/:id/rollback
POST   /v1/machines/:id/promote      {domain?} → service
POST   /v1/volumes                   create JuiceFS volume
GET    /v1/volumes                   list
GET    /v1/hosts                     fleet view
POST   /v1/api-keys                  admin: mint {org_id, scopes[]} → the plaintext key, ONCE
POST   /v1/api-keys/:hash/revoke     admin: tombstone a key; no row is deleted
GET    /v1/api-keys?org=             admin: list an org's keys, revoked ones included
GET    /v1/quotas/:org               admin: the org's limits, or the defaults
PUT    /v1/quotas/:org               admin: set them
GET    /v1/usage?since=&until=       admin: what THIS host metered, in unix
                                     seconds; {host_id, since, until, orgs:
                                     {<org>: {machine_seconds, vcpu_seconds,
                                     mib_seconds, volume_gib_seconds}}}. orgs is
                                     never null; the range echoed back is the one
                                     summed. Default window: the last 24 h
POST   /v1/compose/plan              {compose, env} -> {app, steps[]} in Kahn order
                                     over depends_on; `machines` scope. Every
                                     unsupported key in the file comes back in ONE
                                     400 as {error, unsupported:[{service, key,
                                     message}]}
GET    /v1/health                    liveness (unauthenticated); carries
                                     store_version, the sum of this replica's
                                     version vector (0 on SQLite)
GET    /metrics                      Prometheus (unauthenticated)
                                     engine: pilots_uffd_*, pilots_snapshot_*
                                     host: pilots_machines{state},
                                     pilots_wake_seconds,
                                     pilots_checkpoint_durable_seconds,
                                     pilots_s3_ops_total{op},
                                     pilots_s3_op_seconds{op},
                                     pilots_nbd_cache_hits_total,
                                     pilots_nbd_cache_misses_total,
                                     pilots_router_inflight, pilots_slots_free,
                                     pilots_quota_refusals_total{quota}
```

Every read is scoped to the caller's org. An id another org owns answers
**404**, never 403: existence must not leak across tenants. An `admin` key
sees every row and may narrow a list with `?org=`; a non-admin's `?org=` is
ignored rather than refused. Creates take the org from the authenticated key
and never from the request body. A create refused by a quota answers **429**
with `{"error":"quota exceeded","quota","limit","used"}`.

A host's own calls to a peer's internal listener — the arbiter waking,
suspending or redeploying a machine another host holds — carry the fleet peer
token, derived from `PILOT_AGENT_TOKEN_SECRET`, and are accepted only on a
request that also carries the forwarding marker, which the public listener
strips. The marker has exactly one name fleet-wide (`router.ForwardedHeader`):
the internal listener refuses a request without it, the public listener
deletes it, and both the router's proxy and a host's direct peer call set that
one name — a second spelling anywhere means every forwarded call is refused
before a handler sees it, and means the strip protects nothing. A request
forwarded to a service's arbiter carries the same marker but authenticates
with the caller's own bearer, so it needs no peer token.

### `.internal` service discovery and guest-to-guest traffic

A guest cannot address a peer directly: invariant 5 gives every one of them the
identical `169.254.0.21/30` with gateway `169.254.0.22`, which is exactly what
makes a snapshot restorable anywhere. So discovery and reachability are both
host-mediated, and everything host-specific lives where restore rebuilds it.

**Names.** `<machine>.internal`, resolved by a DNS responder hostd binds on
`169.254.0.22:53` inside each machine's netns. Answers come from the local
Corrosion subscription cache, scoped to the querying machine's `app` and
filtered to healthy. Everything else forwards upstream. The guest's
`/etc/resolv.conf` is written at rootfs build time (post-export, beside the
other fixups) and names only `169.254.0.22` — a constant, so snapshot-safe by
construction.

**Machine addresses are derived, never allocated.** A cluster-wide subnet
allocator would be a control plane, which rule 1 forbids. Instead each host
derives a second, domain-separated prefix from its own WireGuard key:

```
host    fd cc + pubkey[0:14]          /128   (unchanged; hostd listens here)
machine fd cd + pubkey[0:12]          /112   machines at <prefix>::1 .. ::400
```

The machine prefix spends 16 bits of key material on the slot index, leaving
96 bits of derivation entropy — a birthday collision sits past 2^48 hosts
against a fleet of tens, and the slot index is the one the netns pool already
allocates. Two prefixes rather than one widened prefix is deliberate: it makes
the tenant boundary structural. **Guests may only ever reach `fdcd::/16`;
hostd only ever listens under `fdcc::`.** One static rule enforces that, with
nothing to reconcile and nothing to get wrong on a host added next year — where
an enumerated deny ("block `::0` and the host service ports inside every
machine prefix") would have to stay correct on every host forever. A peer
therefore carries two `AllowedIPs` entries.

**The data path.** Guests speak IPv4 and the mesh is IPv6, so the two are
bridged at the host rather than wished away. Every guest additionally holds a
constant `fdee::21` — identical on every guest for the same reason `.21` is,
and equally snapshot-safe. DNS answers the peer's `fdcd` address, and the
**namespace** NAT66s both ways: source-rewrite outbound `fdee::21` to
`<prefix>::<slot>`, destination-rewrite inbound back to `fdee::21`. The root
namespace routes and filters and never translates — it carries one
`<prefix>::<slot>/128 via <veth-peer> dev veth-N` route per machine, the same
shape as the `10.11.x.y/32` route already beside it. Cross-host the packet
routes over the mesh to the owning host and is translated in the target
namespace there. Same path either way.

**Translating in the root namespace instead cannot work**, and the reason is
worth keeping because the arrangement looks natural until it is tried. After
translation the address is `fdee::21`, which is the same address in all 1024 of
a host's namespaces. Netfilter makes the routing decision *after* the
prerouting DNAT and does not carry the ingress interface into it, so both the
inbound direction and the reply to an outbound flow arrive at a thousand
candidate veths with nothing to choose between them. Recovering the answer
needs a connmark and a policy-routing table per namespace: a second addressing
scheme layered on the derived one, reconciled on every machine that moves.
Translating one hop earlier means the packet already carries an address that is
unique on the host by the time anything has to route it.

**Tenant isolation belongs at that root-namespace hop.** Every guest sources
from the same `169.254.0.21`, so classification by source address is impossible
by construction — it is by ingress veth (→ machine → app). Doing it there costs
one rule set per host, updated once per fleet change; doing it inside each netns
would cost a set of every peer address in the app, in up to 1024 netns per host,
re-reconciled on every fleet change and churning hardest during a rescue, which
is precisely when the host is busiest.

The same rules drop, per veth, any IPv6 whose source is not that slot's own
machine address. By the time the root namespace sees a packet the source *is*
unique, so against the ingress-veth match this looks redundant. It is not.
Without it a guest can put a peer's address in packets it sends: the reply goes
to the real owner so it steals no traffic, but the receiving machine's filter
sees a connection opening from inside its own app and accepts it. The ingress
veth is the host's own knowledge; a source address is the guest's.

**Guest-to-guest traffic is activity.** Nothing a peer sends over NAT66 passes
the router, so the root namespace keeps two counters per service replica beside
the tenant filter: a counted drop on a suspended replica's kept address, whose
rising count is the wake, and a counted pass-through on a running replica's
address, whose rising count touches `last_activity`. Open sessions come from
conntrack, which the established-accept rule already loads: an ESTABLISHED TCP
flow to a replica from a machine that is running holds the replica up, however
long it is silent, and stops holding it the tick after the client suspends. A
health probe never counts, because it dials the veth's host-side IPv4 address
and never crosses this hook.

Landmines:

- **Near-zero TTL, always.** A rescued machine lands on a new host with a new
  slot, so its address changes. A guest holding a 300s answer talks to nothing.
- **TTL does not save an established connection.** A pool holding an open
  socket to a rescued machine's old address simply breaks. That is what every
  failover does, but it means `.internal` clients need reconnect logic, and
  "no human action" covers the platform, not the application's connections.
- **Nothing here may enter a snapshot.** Routes, NAT66 and the filter are netns
  and root-namespace state, rebuilt at restore. The guest knows `169.254.0.22`,
  its own `fdee::21`, and whatever DNS returned.
- **Key rotation is a readdressing event.** Machine addresses derive from the
  host's key, so rotating it moves every machine that host runs: drain first,
  or accept a connection-reset event for all of them plus an AllowedIPs and
  NAT rebuild.

### Environment and secrets

Corrosion replicates every row to every host, so nothing secret may be written
to one in the clear. The reference implementation gets this wrong in an
instructive way — uncloud stores each container as JSON embedding the resolved
env and inline config bodies, gossiping both fleet-wide.

`secret://name` references are resolved **client-side**, before any spec is
built, so the value never enters the repo. `POST /v1/compose/plan` therefore
returns a step's `secret_refs` as NAMES and never values; the CLI resolves them
from the operator's own store and sends `secret_env`, which hostd seals. The CLI sends plaintext to hostd
over TLS; hostd seals it with a fleet key from `/etc/pilots/config` before the
row is written. Non-secret values live in `services.env`, sealed ones in
`services.env_sealed`. No plaintext in a gossiped row, none in object storage.

State the limit rather than implying more: this defends against gossip spread
and against anything that reads a database file or a backup — **not** against a
compromised host, since any host with the fleet key decrypts any org's
secrets. Sealing per-host to the owner's key would break self-heal, because the
rescuing host would need plaintext it cannot obtain. Real untrusted-host
secrecy needs a KMS, which is a control plane, which is a rule-1 conversation.

**Key custody is a stated exception to invariant 3.** The fleet key is
operator-held, supplied out of band to `host-bootstrap.sh`, and lives only in
`/etc/pilots/config`. Wipe every host and the sealed values are unrecoverable
with object storage fully intact — so it is the one piece of state whose
durability is the operator's job, in the same trust class as the SSH key that
runs the bootstrap. Rotation requires a re-seal sweep over the affected rows.

### Guest-agent protocol (inside every VM, port 3001)

`GET /health` · `POST /init {timestamp_nanos}` (sets CLOCK_REALTIME — kvm-clock
covers MONOTONIC; without this poke a restored guest's TLS/cron/JS clocks are
frozen at snapshot time) · `POST /exec` (buffered; `bash -c`; default user
uid-1000 = `sprite`, home `/home/sprite`, Node 24 on PATH; root opt-in) ·
`GET /exec/stream` WS — binary frames, **byte 0: 1=stdout 2=stderr 3=exit
(payload[0]=code)**; the verdict goes out as a text
`{"type":"exit","exit_code":n}` FIRST and the binary `3` after it, because a
client settles on the first verdict it sees and only the text one is
untruncated (a signal death is -1, which one byte reports as 255); client to
server **0=stdin 4=stdin_eof**, read only when the
stream was opened with `stdin=true` (the default is `stdin=false`, the
agent-runner path, where nothing is read from the socket and a `0` frame sent
anyway is ignored); single write-mutex · `GET /terminal` WS (pty, JSON frames) ·
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

**A machine is pinned to the template it was built from.** Its memory and disk
images are diffs, and a diff's unchanged ranges name a logical offset rather
than bytes, so they only mean anything against the exact build they were
encoded against. The machine row therefore records `template_mem_build_id` and
`template_rootfs_build_id` at create, and every later capture and restore uses
*those* — never whichever template the acting host happens to hold. The two
differ routinely: the golden template is rebuilt, or a host that has never held
one mints its own with fresh ids. A host restoring a machine whose template it
lacks materializes that template from object storage, which is a download
because builds are content-addressed. `block.SetParent` verifies the pairing
and refuses a mismatch, turning what would be a silently stitched guest into a
failed restore.

**Put the machine store on a filesystem that can share extents** (btrfs, or
XFS made with `-m reflink=1`). Create copies the golden template, and
checkpoint copies the snapshot and the cow *inside the pause window*; all
three are budgeted as metadata operations, and a reflink is what makes them
metadata operations.

Measured on ext4 with the 2GiB golden rootfs, the engine's actual copy
(`cp --reflink=auto --sparse=always`, which skips zero blocks) costs **134ms
warm and 465ms cold** — not free, but not fatal either: create measures
**~1020ms** there, inside its 1.5s budget. What does break is the **checkpoint
pause**, which stops being independent of machine size and measures **409ms to
2172ms p50 across hosts against a 500ms budget**, with single samples past 4s.
That size-independence is the property that makes checkpoints usable at all,
so it is the reason to care.

Nothing errors when extents cannot be shared. So hostd probes at startup,
warns, and reports it on `/v1/health`; `host-bootstrap.sh` prints it and
refuses to finish under `PILOT_REQUIRE_REFLINK=1`. The e2e battery holds
create and wake to the engine targets on every host, and gives the checkpoint
gap a *degraded ceiling* where extents cannot be shared rather than dropping
the assertion — an assertion that stops asserting is how a real slowdown
hides.

Above those two tiers sits a third. `PILOTS_E2E_METAL=1` holds the host to the
**metal SLOs** — create < 500ms, wake < 200ms, checkpoint resume gap < 500ms,
release restore < 1s, promote < 1.5s — which is the latency the product is
sold on and is only achievable on dedicated hardware. The switch is explicit
rather than inferred, because extent sharing is necessary for those numbers
and nowhere near sufficient: a nested-virtualisation laptop node on btrfs
reports `reflink: true` and cannot create a machine in 500ms. Setting the
flag on a host whose `/v1/health` does not report `reflink: true` is a FAILED
step, never a quiet downgrade to the laptop ceilings.

(An earlier note here put the ext4 penalty at 2.2s per create. That measured
`cp --reflink=auto` without `--sparse=always`, which is not what the engine
runs; the same copy without sparse detection takes 19s on these hosts.)

**Snapshot (suspend/checkpoint).** The order of these steps is the design,
and every one of them was arrived at by measuring a resume gap:

1. *Before* pausing, wait for the previous snapshot's background capture.
   That capture reads the whole memory image, chunkifies it and uploads it;
   overlapped with the next snapshot it competes with Firecracker's snapshot
   write, and that write is inside the pause — so the freeze a user feels
   belongs to the checkpoint before the one they asked for.
2. *Before* pausing, ask the uffd handler to make the guest's memory
   resident — **but only when the snapshot will be a Full**. Firecracker
   reads all of guest memory to write a Full, so any page still lazily backed
   faults through the handler with the guest frozen: on a first checkpoint
   that is the entire image, 5.8s versus 450ms. Under a Diff the dirty set IS
   the resident set, so prefaulting first turns the Diff back into a Full and
   costs the whole lever.
3. Reclaim inside the guest over the agent, in one exec: `fstrim`, `sync`,
   `drop_caches`, `compact_memory`. The `sync` is what makes the memory and
   disk images agree about recent writes; the rest shrink what the snapshot
   has to carry at all. Only `sync` is required — the others are tolerated
   individually, because a guest missing one knob still wants the rest.
4. PATCH `/vm Paused` → PUT `/snapshot/create`, **`Diff` whenever the local
   `mem.bin` is exactly `mem_size_mib`, else `Full`**. Firecracker merges a
   diff into that file in place under exactly that condition and OVERWRITES
   it with a partial image otherwise — which does not fail, it silently
   produces an image whose untouched pages read back as zeros and loses the
   machine's memory one restore later. So the first snapshot of every machine
   lifetime is Full: a woken machine has no local image, because suspend
   removes it. `track_dirty_pages` stays off; the diff comes from `mincore`,
   which is the only flavour that composes with hugepage backing, since
   dirty-page tracking forces KVM to 4KiB page tables. Measured on FC 1.16.1
   over a uffd-backed 512MiB guest: 78-116ms against 2.8-3.5s for a Full of
   the same paused instant, with the merged image byte-identical to it.

   **But that 36x does not survive the sequence in front of it, and the
   reason is worth knowing before anyone quotes the number.** mincore reports
   RESIDENCY. Step 2 above prefaults every page before a Full, and nothing
   evicts a page installed through userfaultfd -- so from a machine's first
   checkpoint onward mincore reports all of memory as resident, and every
   later Diff writes nearly all of it. End to end that is 412ms against
   295ms, a ratio of 1.4x. The Diff is never worse than a Full and it is what
   makes an O(dirty) pause POSSIBLE, but the pause is O(dirty) only for a
   machine whose memory is not already fully resident. Closing that gap needs
   a dirty set that is not residency -- which upstream Firecracker offers
   only through track_dirty_pages, and that cancels the hugepage lever. It is
   the one place this design is knowingly leaving a win on the table.
5. Read the NBD handler's dirty bitmap over its control socket, while the
   guest is still paused — a bitmap read mid-write describes a disk state
   that never existed.
6. *Checkpoint*: reflink the cow, **resume immediately**, then chunkify and
   upload in the background (semaphore, default 1 — unbounded chunkify OOMs
   hosts). The memory image is chunkified IN PLACE: step 1 guarantees
   Firecracker cannot overwrite it first, and reflinking half a gigabyte
   pins extents whose allocation cost resurfaces as a multi-second pause a
   checkpoint or two later.
   *Suspend*: chunkify both synchronously, upload, kill the VM, then upload
   the fault order — the handler writes it as it runs, so it is only
   complete once that process is gone.
7. Durability is signalled by marker files and exposed through
   checkpoint-status. `.chunked` means the builds exist on THIS host, which
   is all a local rollback needs; `.durable` means they are uploaded, which
   is what a restore anywhere else needs. Collapsing the two would make
   every rollback wait for an upload it never reads.

- The vmstate is uploaded BEFORE the machine is killed: the kill removes the
  jail it lives in.
- Skip the rootfs build entirely when the machine wrote no blocks.
- The checkpoint response reports `resume_gap_ms`. The call takes longer than
  the freeze — steps 1–3 run with the machine serving — so a client timing the
  round trip overstates what its users experience.
- Never global `sync()`: it holds the kernel bdev lock, and concurrent
  suspends serialize behind it for minutes.

**Chunkifying a copy-on-write file requires its dirty bitmap.** A cow cache is
sparse, so a block the guest never wrote reads back as zeros — byte-identical
to one it deliberately zeroed. Diffing it against its template records every
untouched block as "zeros, mine", and the restored rootfs mounts empty with
nothing having errored. The bitmap lives in the handler's memory and crosses
to hostd over the control socket; inferring it from the file's allocated
extents is NOT a substitute, because a filesystem may report data where there
is a hole.

**A create is a restore.** Each host builds a golden template once — boot the
rootfs, let systemd settle for 20s, chunkify the memory — and every machine
after that starts from it. Booting a kernel instead takes twenty seconds and
produces a machine indistinguishable from this one. The template's disk build
is the golden rootfs chunked directly, never a snapshot of the booted machine:
the two must be the same bytes, or every restore starts from a disk its memory
image does not describe.

**Firecracker is chrooted**, so it reaches neither `/dev/nbdN` nor the fault
socket by their real paths. The device is reproduced inside the jail with
`mknod` at the same baked path a rootfs file would occupy — that path is
recorded inside every snapshot, which is what keeps a snapshot restorable on
any host — and the fault socket is created inside the chroot.

**Restore (create-from-template, wake, checkpoint-restore — same path):**
prefetch snap.bin + prefetch.txt from S3 if absent → netns setup and
uffd-handler spawn run in parallel goroutines (saves 150–250ms), router
port-binding overlaps FC start → PUT `/snapshot/load {mem_backend: Uffd
socket | File}` with `resume_vm:false` → PATCH `/vm Resumed` → async POST
guest `/init {now}` (5ms retry loop, 15s deadline).

**Lazy memory (uffd handler):** receives the uffd fd via SCM_RIGHTS on FC's
socket + a JSON region map `{base_host_virt_addr, size, offset, page_size}`;
serves guest page faults with `UFFDIO_COPY` (`0xC028AA03` — struct
uffdio_copy is 40 bytes, and that size is baked into the ioctl number; the
`_UFFDIO_*` nr values are NOT sequential from zero, `_UFFDIO_API` is `0x3F`);
EEXIST is success, a short copy is an error, and the EAGAIN retry is
**bounded** — an unbounded one turns a persistent EAGAIN into a worker
spinning on a core with a guest thread blocked behind it. 4 fault workers.
**Coalesced prefault**: one bulk `GetRange` of the packed data file in a
goroutine — without it, cold restore = one ~50ms S3 round-trip per 4KiB fault
(~70min for 256MiB). Records fault order to prefetch.txt for next-restore
replay (read the replay file fully BEFORE creating the record file — commonly
the same path, and os.Create truncates). A control socket exposes `prefault`,
which installs every page; the snapshot path calls it before pausing.
MINOR and WP faults are counted and still answered — leaving one unresolved
wedges the guest thread that raised it. Mixed page sizes are refused at the
handshake, not per fault; a **uniform** page size is accepted at either 4KiB
or 2MiB, which is what lets guest memory be hugepage-backed. A short
`UFFDIO_COPY` is resumed from the bytes the kernel reports rather than treated
as failure — a hugetlb copy can be preempted mid-page and is never
redelivered, so failing it wedges the guest thread for ever. The replay set is
the recorded fault order followed by the ranges the last cycle's diff stores
itself (capped at 64MiB): a sequence that matches real access order, then a
set with no ordering.

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
Issue `NBD_DISCONNECT` ioctl BEFORE killing a handler, from the PARENT —
a handler blocked in `NBD_DO_IT` never reaches its own cleanup, and FC then
blocks in D-state with `/dev/nbdN` dead until the host reboots.
**`NBD_DO_IT` must be issued with signals masked on a locked thread.** It
parks in `wait_event_interruptible` for the life of the device, and any
signal delivered to that thread makes the kernel run `sock_shutdown` and
`nbd_clear_que` — so the Go runtime's own `SIGURG` preemption killed roughly
one restore in four. The symptom is thoroughly misleading: the attach
succeeds, the kernel logs a capacity change, the size then returns to zero,
and the caller fails on a sizing timeout against a device that reports no
owner and looks free. A failed `NBD_DO_IT` must also be surfaced rather than
collected at the end, or nothing anywhere names the cause. Devices are handed
out **round-robin**, not lowest-free: the kernel publishes and clears a
device's size asynchronously, and those two are not ordered against each
other.

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

**The golden template stops short of starting the application.** A create is a
resume, so PID 1 and everything under it are already running when the machine
appears — and you cannot inject environment into a running process. Docker's
`environment:` semantics assume the process starts with its env block
populated; there is no such moment here unless one is made. So the template
settles the *base system* only, and the guest agent execs the application after
env delivery, riding the `POST /init` poke that already fires post-resume.

The asymmetry matters and is easy to get backwards: **delivery-and-exec happens
on create from template, never on wake.** A wake resumes a snapshot in which
the application is already running; re-execing there would restart the very
process the guest just restored. Changing an env var therefore takes effect
only through an explicit restart or roll of the machine — never through a
suspend/wake cycle, which must leave the running process and its env untouched.

**Router (in hostd):** TLS termination — the wildcard arrives by ACME DNS-01
through the Cloudflare API and is managed eagerly on every host, custom
domains arrive by on-demand HTTP-01 gated by `certs.Decider`, and TLS-ALPN is
off because the challenge would have to be answered by whichever host DNS
picked → hostname parse (`name` | `port-name` | custom domain) → local
Corrosion lookup → if running-local: proxy into the netns (in-process Go
proxy — no socat, no per-VM forwarder processes); if running-remote: proxy
over WireGuard to the owning hostd; if suspended and `autoStart`: **hold the
connection**, restore locally (or trigger the owner), then proxy. Touch
`last_activity` on every request AND every exec. Idle monitor suspends when
BOTH the wall-clock timer (default 60s, per-machine) and concurrency
(in-flight = 0 against `softLimit`) say idle — exec/WS activity counts, so an
agent mid-build with zero HTTP traffic is never suspended. That monitor owns
sandboxes only. A machine with a release (a rollout's replica or a promoted
sandbox) is the autoscaler's: the host that HOLDS the replica gives it back
when its own in-flight count, its held sessions and the row's `last_activity`
all say idle for the scale-down window and the floor allows it, and the
arbiter alone adds capacity. Every host ranks the same replicated rows the
same way, so only one host ever concludes it is the one to act. A suspended
machine holds no host memory and runs no process; what it costs is the
storage its snapshot occupies. N-replica: round-robin among healthy replicas,
`softLimit` overflow starts the next stopped replica, excess capacity suspends
them down to `minMachinesRunning`, which defaults to zero; `autoStop: off` on
a deploy means never.

**Self-heal:** every hostd heartbeats `hosts.last_seen`; a host silent
>30s is dead; each survivor rescues the slice
`hash(machine_id) mod live_hosts == my_index` — recreate from the machine's
latest builds in S3, write the new `host_id`, URL unchanged. No leader, no
election. Placement double-booking is prevented by hosts being final
authority on their own capacity (a create/rescue targeting a full host is
refused and re-hashed).

**Operations:** a host whose local replica is corrupt or hopelessly behind is
re-seeded with `scripts/corrosion-reseed.sh <ip> --from <survivor>`, which
stops hostd FIRST (the reaper kills any Firecracker with no row for 60s, so
running it against an empty replica would destroy the machines the re-seed is
trying to save), moves the store aside rather than deleting it, and waits for
every table's count to agree with a survivor. Every hostd re-publishes its own
`machines` and `volumes` rows on start, read from the replica and never from
local disk — a write that never gossiped self-corrects on the next restart,
and a row the replica no longer shows may belong to a rescuer, so disk is the
wrong source. `corrosion.service` runs `Type=notify` with a watchdog and hard
memory bounds: a hung state daemon is worse than a dead one, because a dead
one restarts. **No data-plane unit orders on another.** hostd already waits
for corrosion's schema in code, so `After=corrosion.service` buys nothing and
costs the failure shape where one blocked unit wedges the whole data plane.
Anything a customer saw, every self-heal and every re-seed is written up in
`docs/incidents/`.

**Build path:** `POST /v1/builds` accepts a Dockerfile context → BuildKit
(rootless buildkitd on each host) → **BuildKit's `tar` exporter**, which
already emits the flattened filesystem, so no layered image is unpacked →
`mke2fs -d` into an ext4 → chunkify as a generation-0 template build → S3.
Structured NDJSON log stream (`{step, stream, line, ts}`) so an agent can
parse failures and loop.

`mke2fs -d` takes the tarball directly, reading uid, gid and setuid straight
out of the tar headers. That is why the fixups are appended to the tarball
rather than merged into a directory: an unprivileged unpack loses all three
and yields a rootfs where nothing is root-owned. Tarball input needs
e2fsprogs built with libarchive, probed at startup, with a `fakeroot`
extract-and-pack in ONE session as the fallback.

The fixups are what Docker does for a container and the kernel does not do
for a VM: `/etc/resolv.conf` (which cannot be written in a Dockerfile —
BuildKit bind-mounts over it), the guest agent plus its placeholder token,
and `/sbin/init`. **Most real base images carry no init at all**
(`node:alpine`, `python:slim`, distroless), so `/sbin/init` becomes the
guest agent, which mounts the pseudo-filesystems, remounts `/` read-write
and reaps orphans before serving. Images that do carry systemd keep it, and
the agent runs as a unit with `systemd-networkd-wait-online` masked.

The `tar` exporter carries the filesystem and **no image metadata** — no CMD,
ENTRYPOINT, WORKDIR or ENV. That is the price of taking the flattened
filesystem instead of a layered image, and it leaves the agent nothing to
exec once env has been delivered. So the build reads the start spec out of
the **Dockerfile's final stage** and writes it into the image at
`/etc/pilot-agent/start.json`, recording `from_dockerfile_only: true`. Read
that field: a Dockerfile that inherits its command from its base image yields
an empty spec, and a consumer must be able to tell that from "this
application declares no start command" and fall back to the service spec.

**A machine with its own image, or with a volume, BOOTS rather than
restoring.** Both are forced. The golden template's memory describes the
golden template's disk, so resuming it against another root filesystem is a
guest whose memory and disk have never met; and a drive cannot be added to a
snapshot being loaded, while a template captured *with* a volume drive would
carry that drive's capacity in its device state and therefore fit exactly one
volume size. Such a machine records the **nil** memory build to mean "no
parent" — an empty value already means "an old row, use the host's template",
which for a booted guest resolves its pages from a different machine. Its
first suspend captures its own memory; every wake after that is the ordinary
instant restore.

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

The fleet-wide layer cache is keyed **server-side on the Dockerfile's content
hash**, so a client passes no cache name at all — and every app built from the
same scaffold Dockerfile shares one partition, which is why the first deploy of
a new webjs app is already warm on any host.

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

**Volumes:** one JuiceFS filesystem per volume, holding a single raw ext4
image handed to the guest as a **second virtio-blk drive**. Not virtio-fs:
Firecracker does not have it, the upstream request is open and the p9
implementation was rejected on security grounds — the devices available are
virtio-blk, virtio-net, virtio-vsock, balloon and rng. Chunks live in the
same S3 bucket under `volumes/<id>/`. The metadata engine is a **local SQLite
file, one per volume**, kept durable via Litestream→S3 (no central metadata
service; a shared Redis or Postgres would be exactly the control plane rule 1
forbids, and would make every volume unavailable when one box dies). SQLite's
single-writer constraint is a feature here: it enforces one-host-at-a-time
mounting, which the volume row's owner also guards.

Two flags carry the durability and both fail silently if they drift. The
drive's `cache_type` must be `Writeback` — Firecracker's default, `Unsafe`,
does not advertise the VirtIO flush feature at all, so a guest `fsync`
returns success with the data in the host page cache. And `juicefs mount`
must NOT be given `--writeback`, which acknowledges a write before it is
uploaded. A host taking a volume over runs `litestream restore` **before**
mounting: a stale local metadata database mounts without complaint and comes
back missing whatever the previous host wrote.

Volume machines pin to hosts only while mounted; on host death the volume
remounts wherever the machine is rescued (per-write durability — this beats
checkpoint-granularity disk state and is where application data belongs).

**A service that mounts a volume runs one replica, and a deploy replaces it in
place.** `POST /v1/services` takes `volume` (create-only, replicas 1). One
machine mounts a volume, so a service bound to one (the `service_volumes` row)
has exactly one machine, and a rollout boots that same machine from the new
image (the redeploy route): same row, same URL, same claim, no snapshot, no
second machine. A redeploy is **refused (409) while the machine has any
checkpoint**: it repoints the row's template at the new image and clears the
machine's local cache, so a checkpoint taken against the old one could only be
restored as a diff against a base it was never taken from. Destroy deletes the
checkpoint rows and discards their builds; a redeploy keeps the machine, so it
names them and says to delete them first. The window between the kill and the health gate is the price of
one volume; an HTTP request arriving inside it is held on the machine's lock
until the new process serves, and over `.internal` the name has no address
until then, exactly as across a rescue. On a failed gate the machine is put
back on the previous release, on the same volume, with whatever the failed
release wrote. Suspend keeps the claim and the mount, and a volume-backed
replica takes the ordinary floor of zero; only a destroy or a self-heal claim
moves a volume. A promoted volume-backed sandbox's release is the image it was
created from. Availability across a deploy or a host death needs a volume per
replica and application-level replication, Fly's answer too (at least two
volumes per app), and is not built here.

**Databases are the documented exception, and it is a default rather than a
prohibition.** Per-write durability means an S3 round trip per fsync, which a
Postgres data directory pays on every commit. So a database template *defaults*
to a fast local data directory with WAL shipping (`archive_mode=on`,
`archive_command` pushing to object storage, `archive_timeout=60`), giving
point-in-time recovery with an RPO bounded by `archive_timeout` — seconds, not
zero. A volume-backed data directory remains valid and supported where
per-commit durability is genuinely worth the latency: low-write, high-value
data. State which one is in use; do not imply the default is strictly better,
because it trades RPO for commit latency.

One consequence to carry into operations: **a rescued database has a different
RTO from every other machine.** Everything else restores instantly from its
snapshot; a database restores and then replays WAL.

---

## Authentication (first-class GitHub login)

- **Humans:** dashboard sign-in = **Login with GitHub** (webjs
  `createAuth({ providers: [GitHub] })`; handlers at
  `app/api/auth/[...path]`; `AUTH_SECRET` env). No passwords stored, ever.
- **CLI:** `pilot login` runs the **GitHub device flow** (prints a code +
  URL, polls) → `POST ${PILOT_DASHBOARD_URL:-https://pilots.run}/api/cli/exchange`
  with `{github_access_token}` returns `{api_key, org_id, scopes}` → stored at
  `${XDG_CONFIG_HOME:-~/.config}/pilots/credentials` as
  `{api_key, api_url, org_id, secrets}`, directory `0700` and file `0600`
  (a file readable by another user is refused, naming the path). That route
  verifies the token with GitHub's check-a-token endpoint
  (`POST /applications/{client_id}/token`, HTTP basic with the App's own
  credentials), never `GET /user`: `GET /user` accepts a token issued to ANY
  OAuth app, so a token leaked from an unrelated application could mint
  pilots keys for its owner. The CLI ships the App's public `client_id` and
  NO client secret, which is what the device flow exists for. Headless
  fallback: `pilot login --token` / `PILOT_API_KEY` env. **No command
  validates a cached key**: once the file exists every command talks only to
  the fleet, so a dashboard outage cannot take the CLI down with it.
- **Machine auth:** every hostd request carries `Authorization: Bearer
  <api-key>`. Key **hashes** live in the Corrosion `api_keys` table, written
  by whichever host serves the `POST /v1/api-keys` that minted them, so
  **every host authenticates locally** with no auth-service round-trip —
  auth survives any host loss, including the dashboard's. The dashboard is a
  guest on the platform and guests reach only `fdcd::/16`, so it mints keys
  through the public API like any other client, not through its own host.
  The first key on a fleet comes from `hostd bootstrap-key`, run on the box.
  Revoking a key writes an `api_key_revocations` row and deletes nothing: a
  delete racing a replica that still carries the insert loses, and the key
  comes back alive.
- **Agents are first-class principals:** an API key is all an agent needs;
  scopes on the key bound what it can do, stored comma-separated and sent as
  a JSON array. They nest — `machines` ⊂ `deploy` ⊂ `admin`:
  `machines` covers `/v1/machines`, `/v1/checkpoints`, `/v1/volumes`,
  `/v1/sprites` and `/v1/hosts`; `deploy` adds `/v1/builds`, `/v1/services`
  and `/v1/domains`; `admin` adds `/v1/api-keys`, `/v1/quotas` and
  `/v1/usage`. An unknown path or an unknown scope name fails closed, and a
  refusal is `403 {"error":"scope <s> required"}`. The MCP server reads the
  same credentials file/env.
- **WebSocket clients** may carry the key as the subprotocol
  `authorization.bearer.<key>` instead of the header, which a browser cannot
  set on an upgrade. hostd echoes the offered subprotocol on the `101` —
  the WHATWG algorithm fails a connection whose client offered subprotocols
  and whose server picked none — and strips it before the request reaches
  the guest. `?token=` is deliberately not accepted: it lands in logs and in
  shell history.
- Per-machine **agent tokens** (guest exec auth) are minted at create,
  hashed into the machine row, never reused across machines.

---

## Crisp compatibility (the reference customer)

Non-negotiables, honored by the contracts above: stable per-machine URL
across every lifecycle event · multiple named checkpoints with **in-place**
restore (same row/URL/token — never respawn-from-template) · WS streaming
exec with `stdin=false` for `claude -p … --output-format stream-json` ·
`cwd` + `env` on every exec (buffered and streaming). The sprites byte frame
protocol (1/2/3, plus 0/4 client to server) is kept exactly so crisp's client
code drops in, and `stdin=false` stays the default so a client that never
writes cannot hang on a process holding an open stdin.

Three things together are what make a hand-built sprites client work
unchanged. `GET /v1/sprites/:name/exec` is the name-keyed route such a client
constructs itself, with the key in an `Authorization` header. The guest is the
sprites environment: user `sprite`, home `/home/sprite`, Node 24 on `PATH`, so
an exec that names no user lands where the client expects. And
`@pilots/sdk/sprites-compat` is the drop-in adapter for anyone who would rather
change one import line: a sprite's `id` is the machine's NAME, because a
sprites consumer persists that id and hands it back as a path segment to a
name-keyed route, and `machineId` carries the `m-…` id for anything going
through the typed client.

---

## Monorepo layout (webjs conventions honored)

```
pilots/
  AGENTS.md  CLAUDE.md(@AGENTS.md)  ARCHITECTURE.md   # this plan's content
  package.json            # npm workspaces: apps/dashboard, packages/cli, sdks/js
  apps/
    hostd/                # Go, own go.mod — the entire data plane
      cmd/hostd/  cmd/guest-agent/  cmd/chunkify/
      internal/{fc,block,uffd,nbd,ctlsock,netns,router,state,s3,build,volumes,selfheal}/
      systemd/            # hostd.service, corrosion.service
    dashboard/            # webjs full-stack app (scaffolded `npm create webjs`);
                          #   deployed by `pilot deploy` from apps/dashboard/,
                          #   SQLite on a volume at /data, pilots.run as a
                          #   custom domain, one replica
  packages/cli/           # `pilot` CLI + its MCP server (TS, no build step:
    bin/  src/{commands,compose,mcp}/   #   Node strips the types at run time)
  sdks/js/                # @pilots/sdk — typed client + sprites-compat adapter
  sdks/go/                # github.com/vivek7405/pilots/sdks/go
                          #   both hand-written; both carry a drift test that
                          #   parses internal/api and fails on a wire change
  scripts/                # bash one-shots ONLY: host-bootstrap.sh <ip>,
                          #   build-golden-rootfs.sh, dev-vm.sh, e2e.mjs
```

**Metering is host-local, like everything else.** Each hostd appends one closed
interval per (machine, org, state) to `<machine state root>/usage/<UTC
day>.ndjson`, closed and reopened on every state write it performs and on a
60 s tick, and uploads each day file to `usage/<host_id>/<day>.ndjson` in object
storage — outside the `chunks/` prefix, and what a wiped disk loses is bounded
by one tick. `GET /v1/usage` answers from those files and from the intervals
still open; the dashboard polls every live host once a minute and sums, and
meters nothing itself. **A suspended machine bills storage only:
machine-seconds and volume-GiB-seconds accrue, vCPU-seconds and MiB-seconds do
not.** There is deliberately no replicated `usage` table: a write per minute per
machine gossiped fleet-wide, on a live schema, is hard rule 6 with extra steps.

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
Dashboard owns orgs/users in its own Drizzle DB and mints platform API keys
through `POST /v1/api-keys` on any host, so every host authenticates locally
from its replica; it is deployed ON the platform via promote and is never in
the request path. It cannot reach its own host directly — a guest reaches
only `fdcd::/16` — which is why the mint is a public API call rather than a
loopback write.

---

## Verification (continuous)

`scripts/e2e.mjs` is the single growing battery: correctness from Phase 2,
timing from Phase 3, chaos from Phase 4, PaaS from Phase 5, tenancy and
hostility from Phase 6 — later phases never retire earlier assertions. Its
key comes from `hostd bootstrap-key` and must carry the `admin` scope, since
the battery exercises every scope's routes; `scripts/cluster/gate.sh` seeds
the fleet the same way rather than writing `api_keys` by hand. Hostility is
the one phase split across two batteries, because the public API cannot see a
process in D-state, a cgroup's `memory.events`, a file-descriptor count, or a
hostd that was SIGKILLed: the API-visible half (netns churn, egress
containment, capacity refusal, quota parity) is in `e2e.mjs`, and the
host-shell half (the NBD wedge and its deliberate negative control, the
per-host resource counts, cgroup containment, Firecracker API exhaustion,
orphan pile-up) is in `scripts/cluster/gate.sh` as numbered sections.
The battery's exec-stream section drives the
frames, both key carriers, the sprites alias, the `logs?follow` tail across a
suspend, and the guest contract (`sprite`, `/home/sprite`, Node 24) through
Node's global `WebSocket`; the gate streams the same command through every
host that does not own the machine, by id and through the alias. Its edge
section drives a machine that echoes what reached it, so a forged
`X-Forwarded-For`, the API hostname and the per-state machine gauge are
asserted through the router rather than in a unit test; `gate.sh` section 1b
asserts the same hostname on every host and that their replica versions have
not drifted apart. Its data-route section drives the compose plan (the CLI's
own fixture, its ordering, its `secret_refs`, and the one 400 listing every
unsupported key), the service patch and its release list, and the usage ledger
across a create, a suspend, a wake and a destroy — asserting there that a
suspended machine kept accruing wall time and stopped accruing compute; `gate.sh`
section 3 adds a service patch sent to a host that does not arbitrate it, and 3b
that every host answers `/v1/usage` with its own `host_id`. `go test ./...` for netns/block/header/state/s3
(block-layer round-trip + diff-chain tests are mandatory). Drift tests in
both SDKs parse `internal/api` on every
`npm test`. Dashboard: `webjs check` / `doctor --json` / `typecheck` /
`test`. CI runs unit tests + the single-VM e2e on every push, and at a tag
builds the golden rootfs and asserts it matches the committed pin. Phase 6f
adds the metal SLO tier (`PILOTS_E2E_METAL=1`) and the fleet gate; the
production sign-off run is the first `docs/incidents/` entry, and a section
of it marked "skipped" or "laptop only" makes the run red.
