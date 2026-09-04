# uncloud — architecture notes
_Research date: 2026-09-03, against the local clone at commit `1c29d4d` (2026-08-28, 1186 commits since 2024-07-20, essentially one author: Pasha/Pavel Sviderski ~1046 commits, plus Anton Ovchinnikov, Miek Gieben, Justin Bradford). Paths relative to `~/uncloud`. "(inferred)" marks conclusions not stated in code/docs._

## One-paragraph summary

uncloud is a Go daemon (`uncloudd`) plus CLI (`uc`) that turns N Docker hosts into a "network of machines" with **no control plane and no quorum**: every machine runs an identical stack — WireGuard mesh, Docker, an embedded DNS server, an embedded registry (unregistry), Caddy as a global service, and **Corrosion** (Fly's cr-sqlite CRDT + SWIM gossip) as the *only* replicated store (`README.md:19-21`, `AGENTS.md:200-222`). pilots' claim of "borrowed from uncloud" is exact: uncloud *is* a Corrosion user — `internal/machine/store/store.go:25-32` wraps Corrosion's HTTP API; the schema is three tables (`schema.sql:1-42`). The replicated state is deliberately tiny and *observational* — machine identity/network config and the live Docker container list — while the "desired state" lives nowhere durable: `uc deploy` reads `compose.yaml`, inspects the cluster through any one machine (gRPC over SSH, fan-out via grpc-proxy), computes a plan, and drives each target machine directly, one container at a time with health gating (`pkg/client/deploy/deploy.go:292-332`, `strategy.go:60-207`). Asynchronous reconciliation is confined to derived views — Caddy config, DNS records, WireGuard peers, machine-info republish — each a local subscription on the local Corrosion replica. There is no rescheduling when a machine dies, no automatic rollback after a deploy completes, no shared TLS-cert storage, and volumes are host-pinned Docker volumes; the author treats all of these as conscious trade-offs to keep Raft out of the system.

## System map

```
 laptop                                          machine A (identical on B, C, ...)
 ┌──────────────┐  ssh  ┌────────────────────────────────────────────────────────────┐
 │ uc (CLI)     │──────▶│ sshd → `uncloudd dial-stdio` → /run/uncloud/uncloud.sock    │
 │ compose.yaml │       │                     │ (gRPC proxy server, grpc-proxy codec)  │
 │ ~/.config/   │       │                     ▼                                          │
 │  uncloud/    │       │  Director: md["machine"|"machines"] → local sock or           │
 │  config.yaml │       │            [mgmtIPv6]:51000 of remote machines (One2Many)     │
 └──────────────┘       │                     │                                          │
                        │  ┌──────────────────┴────────── uncloudd ───────────────────┐ │
                        │  │ machine API (/run/uncloud/machine.sock + [fdcc::]:51000)  │ │
                        │  │  ├ Machine svc  (init/join/token/reset/inspect)           │ │
                        │  │  ├ Cluster svc  (AddMachine/ListMachines/Remove/DNS)      │ │
                        │  │  ├ Docker svc   (create/start/stop/exec/logs/volumes/img) │ │
                        │  │  ├ Caddy svc    (validate Caddyfile via admin.sock)       │ │
                        │  │  └ Lease svc    (distlock, in-memory per machine)         │ │
                        │  │ clusterController goroutines:                             │ │
                        │  │  ├ WireGuard ctl loop (1s): peer status, endpoint rotate  │ │
                        │  │  ├ machines-subscription → reconfigure WG peers           │ │
                        │  │  ├ Docker events → upsert own containers rows             │ │
                        │  │  ├ machine-info republish (60s + on change)               │ │
                        │  │  ├ containers-subscription → DNS resolver map             │ │
                        │  │  ├ containers-subscription → Caddyfile → caddy admin.sock │ │
                        │  │  ├ DNS server  <machineIP>:53  (*.internal + forward)     │ │
                        │  │  ├ unregistry  <machineIP>:51500 (containerd image store) │ │
                        │  │  └ metrics     <machineIP>:51090                          │ │
                        │  └──────────────┬───────────────────────────────────────────┘ │
                        │                 │ HTTP/2 127.0.0.1:51002 (bearer token)        │
                        │  ┌──────────────▼────────────┐   ┌──────────────────────────┐ │
                        │  │ uncloud-corrosion (docker, │   │ dockerd                   │ │
                        │  │ host net) store.db,        │   │  net "uncloud" 10.210.X/24│ │
                        │  │ gossip [fdcc::]:51001 QUIC │   │  caddy (global svc) :80/443│ │
                        │  └──────────────┬────────────┘   │  user containers 10.210.X.Y│ │
                        │                 │ gossip over WG   └──────────────────────────┘ │
                        │  wg iface "uncloud": 10.210.X.1/24 + fdcc:<pubkey[0:14]>/128    │
                        └────────────────────────────────────────────────────────────────┘
```

| Component | Where | Notes |
|---|---|---|
| `uc` CLI | `cmd/uc/`, `internal/cli/`, `pkg/client/` | Cobra; all cluster ops are client-side plans executed over gRPC |
| `uncloudd` | `cmd/uncloudd/main.go:19-65`, `internal/daemon/`, `internal/machine/machine.go` | systemd unit `uncloud.service`, `Restart=always` (`scripts/install.sh:284-292`) |
| Corrosion | `internal/machine/corroservice/`, `internal/corrosion/` | runs as Docker container `uncloud-corrosion`, image `ghcr.io/unlabs-dev/corrosion:2026.6.15` (`corroservice/docker.go:22-25`); a fork of superfly/corrosion tracking upstream v1.0.0 (`Dockerfile:29`, issue #172) |
| Store schema | `internal/machine/store/schema.sql` | `cluster` kv, `machines`, `containers` |
| Local (non-replicated) state | `internal/machine/state.go:311-338` (`machine.json`), `internal/machine/db.go:256-296` (`machine.db` sqlite: `containers(id, service_spec)`) | machine.json is declared source of truth for the machine's own info (`machine.go:859-860`) |
| WireGuard | `internal/machine/network/` | kernel WG via wgctrl/netlink; design copied from Talos KubeSpan (`peer.go:75-76`) |
| Docker network | `internal/machine/docker/controller_linux.go:226-317` | bridge `uncloud`, per-machine /24, MTU matched to WG |
| DNS | `internal/machine/dns/` | miekg/dns on `<machineIP>:53`, `*.internal` |
| Ingress | `internal/machine/caddyconfig/` + Caddy image as global service | Caddyfile regenerated from store, loaded via `/run/uncloud/caddy/admin.sock` (`machine.go:63-65`) |
| Image distribution | `pkg/client/image.go`, `github.com/psviderski/unregistry` embedded (`machine.go:507-532`) | |
| gRPC fan-out | `internal/machine/api/proxy/` | siderolabs/grpc-proxy, from Talos apid (`remote.go:302-303`) |
| Distributed lock | `pkg/distlock/` | Redlock; added 2026-08-27..28, **no callers yet** |
| Managed DNS | `internal/dns/client.go`, `internal/machine/cluster/dns.go` | `*.<id>.uncld.dev` via github.com/psviderski/uncloud-dns (Route53-backed) |
| Test harness | `cmd/ucind`, `internal/ucind/`, `test/e2e/` | Uncloud-in-Docker: privileged dind containers |

## Shared state without a control plane (the core)

### What is replicated: three tables, JSON blobs inside

`internal/machine/store/schema.sql`:
- `cluster(key, value, updated_at)` — kv: `network` (cluster CIDR) and `created_at` (`cluster/cluster.go:57-62`), `uncloud_dns` (reserved domain + **plaintext API token**, `cluster/dns.go:438-447`, TODO to encrypt at :445).
- `machines(id, name AS json_extract(info,'$.name'), info JSON)` — `info` is a protojson `MachineInfo`: id, name, subnet, management IP, WG endpoints, public key, public IP, daemon/docker/OS versions (`machine.go:1063-1081`).
- `containers(id, container JSON, machine_id, service_id AS json_extract(...labels...), service_name AS ..., sync_status)` — the full Docker inspect + the `ServiceSpec` embedded in labels (`schema.sql:23-36`).

Migrations: none in Corrosion proper; "it's PITA to do schema migration in Corrosion at the moment" (author, issue #31). The 0.19→0.20 Corrosion v0→v1 jump is done by dumping durable rows to a seed JSON and reseeding a fresh store (`internal/machine/corromigrate/migrate.go:56-107`, `236-282`).

### How it is replicated

- Corrosion agent per machine, config generated by uncloudd (`machine.go:643-707`): gossip on `[<mgmt IPv6>]:51001` (`corroservice/config.go:298`), **plaintext gossip** (`machine.go:685`, WireGuard is the encryption layer), QUIC `max_mtu` pinned to 1280-48 to avoid black-holing across heterogeneous WG MTUs (`machine.go:679-684`), `bootstrap` = management IPs of *all* known peers (`machine.go:662-670`, TODO to use a partial list), API on `127.0.0.1:51002` with a bearer token stored in `machine.json` (`machine.go:235-247`).
- uncloudd talks to Corrosion over HTTP/2 with a retrying transport and **resubscribe-from-last-change-id** subscriptions (`internal/corrosion/client.go:36-43`, `148-256`). Every derived view is a `SubscribeContext` on a SQL query returning initial rows + change events (`store.go:270-334`, `container.go:547-623`).
- Consistency model as stated by the author: "favors Availability and Partition tolerance (AP)"; "the *same* state doesn't always mean the intended one from each user's point of view" (`misc/design.md:58-61`, `70-74`). Conflict handling = cr-sqlite LWW per column; nothing in uncloud detects or resolves conflicts.
- Partial replication is a first-class expectation: cr-sqlite can materialise a row before its columns arrive, so every reader skips rows whose JSON is `""`/`{}` and logs a warning (`store.go:196-201`, `286-292`; `container.go:483-489`).

### Who writes what (the implicit single-writer rule)

| Row | Writer | Path |
|---|---|---|
| `machines[self]` | the machine itself, from `machine.json` | `internal/machine/cluster.go:529-563` (`syncMachineInfo`, skip if `proto.Equal`), republished every 60s and on docker restart (`cluster.go:32-34`, `485-518`) |
| `machines[new]` | **whichever machine the CLI is connected to**, at `AddMachine` time | `cluster/cluster.go:87-184` |
| `machines[x]` delete | any machine, at `RemoveMachine` | `cluster/cluster.go:245-270` |
| `containers[*] where machine_id=self` | the machine itself, from Docker events | `docker/controller.go:150-195` |
| `containers where machine_id=X` delete | any machine, at `RemoveMachine(X)` | `cluster/cluster.go:254-259` |
| `cluster.*` | init machine / any machine (DNS reserve) | `cluster/cluster.go:50-64`, `cluster/dns.go:481` |

So the practice is "own rows only" for the two hot tables, with **read-then-write uniqueness checks** for the rest: machine name uniqueness (`cluster/cluster.go:115-117`, `machine.go:1150-1161`), management-IP and public-key uniqueness (`:118-131`), subnet allocation from an in-memory IPAM seeded by `ListMachines` (`cluster/cluster.go:109-161`, `ipam.go:319-340`). The author flags the hole himself: `// TODO: announce the new machine to the cluster members and achieve consensus. We should perhaps not proceed if this machine is in a minority partition.` (`cluster/cluster.go:174-175`). Two concurrent `uc machine add` through different entry machines can allocate the same `/24` (inferred; nothing prevents it). Service-name collision across machines is also a known TODO (`machine.go:1357-1358`).

### Membership and liveness

Machine UP/SUSPECT/DOWN comes from Corrosion's **SWIM membership** read over its admin unix socket (`internal/corrosion/admin.go:131-140`, `215-266`; `cluster/cluster.go:198-242`). The querying machine is always UP (`:231-234`). RTTs also come from the admin socket (`admin.go:268-303`). There is no uncloud-level heartbeat row.

### Join / bootstrap (`uc machine add`)

1. CLI SSHes to the new box, runs the embedded install script (`internal/cli/machine.go:54-99`), waits for `uncloudd` on its unix socket.
2. New daemon boots with a fresh keypair and Corrosion on loopback so it has a store to init against (`machine.go:208-233`, `405-417`).
3. CLI asks the new machine for a `Token` (public key + candidate endpoints + detected public IP, `machine.go:1009-1036`), then calls `AddMachine` **on an existing cluster machine**, which allocates id/name/subnet/management IP and writes the `machines` row (`internal/cli/cli.go` `AddMachine`, lines 65-125 of the function; `cluster/cluster.go:135-178`).
4. CLI snapshots the existing machine's **per-actor version vector** (`crsql_db_versions`, `store.go:63-88`) and passes it as `MinStoreVersion` in `JoinCluster` together with the list of other machines (`cli.go` AddMachine lines 127-158; `machine.go:888-991`).
5. On the new machine, the cluster controller configures WG peers from that list, restarts Corrosion with the mesh gossip address and full bootstrap list, then **blocks store-dependent components until the local replica has caught up** to `MinStoreVersion` *and* `__corro_bookkeeping_gaps` is empty (`cluster.go:192-197`, `369-483`; `store.go:96-117`). The cluster API returns `Unavailable` until then (`cluster/cluster.go:66-74`). This gate exists because of real bugs: an empty machines list once wiped WG peers and locked the machine out (issue #155; safety check at `cluster.go:641-651`, `661-667`).
6. CLI then re-runs the Caddy global deployment so the new machine gets a proxy, and updates managed DNS records (`cmd/uc/machine/add.go` lines 82-167 of `add()`).

### Leave / death

- `uc machine rm`: must be issued through a *different* entry machine (`cmd/uc/machine/rm.go:77-96`); deletes the machine's container rows then its machine row (`cluster/cluster.go:254-266`); if reachable, `Reset` removes containers, WG link, iptables, data dir (`machine.go:1315-1338`, `710-737`; `cluster.go:746-763`). Machine's own store rows are deleted by the *remover*, not by itself.
- Death without removal: nothing happens to its rows. `ListContainers`/`SubscribeContainers` only join on `machines` to drop orphans of *removed* machines (`store/container.go:440-445`, `549-552`). DNS and Caddy resolvers have explicit TODOs to filter by membership (`dns/resolver.go:46`, `caddyconfig/controller.go:133-135`). Its services are simply gone until a human runs `uc deploy`/`uc scale` again: "No automatic rescheduling on host failure (by design)" (author on HN, https://news.ycombinator.com/item?id=46144570). Caddy copes via passive health (`fail_duration 30s`, `lb_retries 3`, `caddyfile.go:34-39`); DNS does not.

### What is deliberately NOT replicated

- Desired service specs (only inside container labels/JSON and the local `machine.db`).
- Env vars/secrets: stripped before the containers row is written (`store/container.go:422-437`) — but `PreDeploy.Env` is not, open bug #422.
- Volumes and images: queried live via One2Many broadcast (`scheduler/state.go:222-253`, `pkg/client/image.go:58-100`).
- TLS certificates: each Caddy has its own store → issue #31 (open since 2025-02, 27 comments).
- Leases: per-machine in-memory (`pkg/distlock/memory.go`).

## Per topic

### 3. "Imperative over declarative"

Author's words: "Favoring imperative operations over state reconciliation simplifies both the mental model and troubleshooting" (`README.md:41-42`); "the user can run a command to start a container on a specific machine. The command can call the target machine directly, handle the errors accordingly, and return the result... the latter [declarative] approach is more complex, less predictable, and has more edge cases" (`misc/design.md:107-111`); but "asynchronously updating the configuration of DNS servers or reverse proxies in accordance to the state changes... is likely a more reliable approach" (`design.md:113-114`); "Maybe for now we should only aim for a simpler and more static container orchestrator where container scheduling can only be initiated by a user... they won't be moved to other machines automatically" (`design.md:120-122`). On HN he concedes reconciliation *does* happen, just "locally during CLI execution, not continuously in background control plane processes" (https://news.ycombinator.com/item?id=46144570).

Mechanics:
- `Deployment.Plan` → validate → resolve spec against cluster domain → `InspectClusterState` (machines not DOWN + volumes) → `Strategy.Plan` → `ServicePlan` of operations (`pkg/client/deploy/deploy.go:292-332`).
- `RollingStrategy.planReplicated`: eligible machines (x-machines + volume constraints, `scheduler/constraint.go:22-49`) are **shuffled** (`strategy.go:88-91`) then sorted so machines already holding up-to-date containers come first (`:132-145`); replicas assigned round-robin (`:148-151`); per slot: `Run`, keep (up to date), or `Replace` (start-first unless port conflict / single-replica volume → stop-first, `determineUpdateOrder`); leftovers → `Remove` (`:190-200`).
- Each operation is an RPC to the *target* machine via `ProxySingleMachineContext`: create → start → `WaitContainerHealthy` with a default 5s monitor period (`operation/container.go:39-68`; docs `website/docs/4-guides/1-deployments/4-rolling-deployments.md:69-74`).
- Rollback = per-container only: failed new container is stopped and kept, old one restarted for stop-first, deployment halts, earlier replacements stay (`4-rolling-deployments.md:155-162`). No post-deploy rollback ("Automatic rollback on failure is coming soon", `README.md:32-33`; "the lack of certain capabilities such as automated rollbacks can be a deal-breaker", Substack). Retry = run `uc deploy` again; it skips up-to-date containers (`:178-181`).
- Global services don't follow new machines automatically (`3-deploy-global-services.md:36-38`); `machine add` re-deploys Caddy explicitly (`add.go` lines 91-164 of `add()`).
- Pre-deploy hooks run as one-off containers before rollout (`operation/predeploy.go`, docs `5-pre-deploy-hooks.md`).

Contrast with pilots: pilots' self-heal *is* a reconciler ("dead host's machines return on survivors, zero human action", pilots `ARCHITECTURE.md:31`), driven by the same per-host-subscription pattern uncloud reserves for DNS/Caddy. uncloud's author explicitly stopped short of this: "Do you really need HA for them when you're perhaps a single user?" (HN 43285730).

### 4. Networking

- Address plan (`misc/design.md:32-37`): `10.210.0.0/16` cluster (`cluster/ipam.go:288`), `/24` per machine (`:286`), machine = first address (`network/ip.go:12-14`), containers `.2-.254` from Docker's bridge IPAM. Management IPv6 = `fdcc:` + first 14 bytes of the WG public key (`network/ip.go:16-22`) — **identical derivation to pilots' host address** (pilots `ARCHITECTURE.md` "host fd cc + pubkey[0:14] /128").
- WG interface `uncloud`; per-peer AllowedIPs = mgmt `/128` + the peer's `/24` (`network/config.go:254-261`); persistent keepalive; peers updated in place, never replaced (`:236-237`, `291`).
- Peer discovery = subscription on `machines` → `configurePeers` rewrites the peer list, preserving the currently-working endpoint (`cluster.go:632-744`). A new machine only needs one existing peer; others learn it through the store (`README.md:246-248`).
- NAT traversal = Talos KubeSpan-style **endpoint rotation**: each machine advertises all routable IPs + detected public IP (`network/address.go:321-395`, ipify/ipinfo/ip-api); a 1s loop reads handshake times, classifies peers unknown/up/down (`peer.go:78-148`), rotates to the next endpoint when down (`peer.go:150-173`, `wireguard_linux.go` `changeWireGuardEndpoints`), and learns the endpoint of a peer that connected *inbound* (`peer.go:54-73`), persisting it to `machine.json` (`cluster.go:338-367`). No STUN, no relay (inferred: no such code); a NAT'd homelab box works because it dials out (issue #155).
- Docker bridge `uncloud` created with the machine subnet, `trusted_host_interfaces=uncloud`, MTU option (`docker/controller_linux.go:269-296`); iptables: accept wg→bridge in `DOCKER-USER`, allow DNS to the machine IP, skip MASQUERADE for bridge→wg so container IPs are preserved end-to-end (`:319-364`; blog `website/blog/2025-08-01-wireguard-overlay/index.md`).
- DNS: server on `<machineIP>:53` (`dns/server.go:21-26`, `108-137`), the Docker network's DNS points at it. Names: `<svc>.internal`, `<svc-id>.internal`, `<machine-id>.m.<svc>.internal`, prefixes `rr.`/`nearest.` (`dns/resolver.go:96-102`, `server.go:129-137`, `68-100`). Answers only **healthy** containers from the local subscription cache (`resolver.go:76-104`), TTL 0 (`server.go:112`), shuffled; `nearest` sorts local-subnet IPs first. Non-`.internal` forwarded upstream with a 1024 semaphore (`server.go:27-31`).
- Cross-machine LB: none at L4 — DNS RR plus Caddy. Caddy runs as a **global** service; every instance gets a Caddyfile with *all* healthy upstreams cluster-wide, local machine's containers first so `lb_policy first` can prefer same-host (`caddyfile.go:115-119`); `lb_retries 3` / `fail_duration 30s` (`caddyfile.go:34-39`). Controller: subscribe containers → fingerprint → generate → validate/load through admin socket → write to disk only after successful load (`caddyconfig/controller.go:80-201`). `x-caddy` per-service Go templates with `{{upstreams [svc] [port]}}` (`template.go`, docs `2-publishing-services.md:140-150`).
- HTTPS: each Caddy does its own ACME → challenge bouncing across machines behind multi-A DNS; "instances failed to issue certificates for days or weeks" (issue #31). Fix in progress; author refuses a Raft/NATS-JetStream dependency: "NOT having Raft in our current design is a critical property I want to preserve" (issue #31 comment).
- Managed DNS: `uc machine init` reserves `<rand>.uncld.dev` (`cmd/uc/machine/init.go:248`), records point only at machines whose Caddy answers `http://<publicIP>/.uncloud-verify` with the machine id (`pkg/client/dns.go:37-84`, `147-218`; `caddyconfig/controller.go:22`, `caddyfile.go:26-32`).

### 5. Placement

Client-side, at plan time. Constraints: `x-machines` names/IDs (`pkg/api/placement.go`, `scheduler/constraint.go:51-69`) and "machine has the named Docker volume" (`:71-124`). Candidate set = machines not DOWN per SWIM (`pkg/client/machine.go:64-72`). Spread = shuffle + round-robin, sticky to machines that already run the up-to-date spec (`strategy.go:88-151`). `ScheduleContainer` heap-based scheduler is `not implemented` (`scheduler/service.go:190-194`); no resource awareness (TODOs `constraint.go:26-27`, `strategy.go:63`). A machine offline during deploy is excluded from candidates but its existing containers still appear in `svc.Containers`, so the plan emits `RemoveContainerOperation`s against an unreachable machine which will fail at execution (inferred from `strategy.go:93-131`, `190-200`).

### 6. Storage

Docker named volumes only, **host-pinned**; "As of April 2025, the volume must be created before deploying a service using it" (`pkg/api/volume.go:29-30`). `uc deploy` runs a `VolumeScheduler` that creates missing volumes on exactly one eligible machine and co-locates services sharing a volume (`scheduler/volume.go:12-33`). Nothing migrates; single-replica+volume forces stop-first (`4-rolling-deployments.md:31-35`). Author on HN 43285730: "Currently Uncloud doesn't handle volume replication... Applications needing HA should handle their own replication." Roadmap 2026: "modern persistent volumes with snapshots and replication" (Substack).

### 7. Unregistry / image distribution

`uncloudd` embeds unregistry, a registry-protocol server backed by Docker's containerd image store, listening on `<machineIP>:51500` (`machine.go:507-532`; port moved from 5000 in 0.20). `uc image push`/`uc deploy` opens a local TCP or unix proxy → SSH tunnel → unregistry, tags the image for `localhost:<port>/...`, runs a normal `docker push` (`pkg/client/image.go:200-410`); the registry protocol's blob-existence check is what makes it "only the missing layers". Requires containerd image store on the target (`image.go:224-231`). Docker Desktop / rootless need a `socat` helper container (`image.go:39-43`, `331-360`). Pushes go to all machines or only `x-machines` targets (`2-deploy-specific-machines.md:248-256`). There is no cluster-internal replication of images between machines — the laptop pushes to each.

### 8. Remote management

- Transport: SSH to any machine → `uncloudd dial-stdio` bridges stdio to `/run/uncloud/uncloud.sock` (`cmd/uncloudd/dialstdio.go:132-147`, `connector/sshcli.go:22-39`); Go-SSH variant dials the socket directly (`connector/ssh.go:74-95`); also `tcp://[mgmtIPv6]:51000` and `unix://` (`1-connecting.md:209-226`). Contexts hold an ordered connection list; all are tried in order (issue #119 → commit `831c581`).
- Auth: SSH + membership of the `uncloud` unix group on the socket (`machine.go:609-641`; `1-connecting.md:172-191`). The mesh gRPC port uses **insecure credentials** (`proxy/remote.go:364-366`) — WireGuard is the trust boundary; "root on any machine is root on the entire cluster" (author, issue #31).
- Fan-out: `md["machine"]` → One2One, `md["machines"]` (names/IDs or `*`) → One2Many with per-response machine metadata injected (`proxy/director.go:42-114`, `mapper.go:225-283`); remote backends cached forever (TODO `director.go:108-111`). CLI/daemon version compatibility enforced by interceptor (`internal/grpcversion/`).
- Distributed lock: Redlock over all registered machines, each node an in-memory lease store reached via the proxy (`pkg/distlock/doc.go`, `pkg/client/locker.go:38-46`), with the documented caveat that membership churn + eventual replication can yield disjoint quorums. Landed 2026-08-27/28, unused so far; intended for cert issuance/#31.

### 9. Failure modes & limitations (author-documented)

- No rescheduling on machine death; pre-deploy replicas across machines instead (HN 46144570).
- No automatic rollback after deploy (`README.md:32-33`; `4-rolling-deployments.md:131-135`: unhealthy containers are pulled from Caddy but "doesn't automatically restart or roll it back").
- Stale containers of a dead machine remain in DNS answers (TODOs `dns/resolver.go:46`, `caddyconfig/controller.go:133-135`); 0.20 fixed only the *removed*-machine case (release notes).
- Split brain: AP by design (`design.md:58-61`); `AddMachine` in a minority partition is a known TODO (`cluster/cluster.go:174-175`); duplicate service names across partitions (`machine.go:1357-1358`).
- Certificates not shared → slow/failed issuance behind multi-A DNS (#31).
- Corrosion upgrades are breaking and painful (#172; whole `corromigrate` package; 0.20 requires upgrading every machine at once).
- Gossip needs `51001/udp` open on the WG interface; UFW-default hosts break joins (#65); IPv6-management gossip timeouts on some setups (#206).
- When uncloudd/Corrosion are down, WG, Docker, Caddy keep serving; internal DNS stops (author, issue #31).
- Secrets: env stripped from replicated rows but `PreDeploy.Env` leaks (#422); managed-DNS token plaintext in `cluster` kv (`cluster/dns.go:445`).
- `uc machine rm` cannot target the machine you are connected through (`rm.go:77-96`).
- Resource-aware scheduling, autoscaling, IPv6 for workloads: absent (#126, HN).

### 10. Testing approach

- **ucind** = "Uncloud-in-Docker": each machine is a privileged `docker:dind` container (`internal/ucind/machine.go:97`) built from `Dockerfile:51-60` with wireguard-tools, socat, and the Corrosion image tarball preloaded so joins don't pull; `mise ucind cluster create -m 3` (`HACKING.md:137-141`); the image must be rebuilt after daemon changes (`HACKING.md:67-68`).
- e2e (`test/e2e/`, ~4.5k LOC) drives the **public client API** through a real 3-machine cluster: `TestClusterLifecycle` waits until *every* machine's replica reports all 3 machines UP before asserting (`cluster_test.go:64-109`); health monitor period set to 0 globally for speed (`main_test.go:10-14`); `TEST_CLUSTER_NAME` reuses a cluster (`cluster_test.go:30-40`). Unit tests cover the strategy planner exhaustively (`strategy_test.go`, `container_test.go` 2k lines) and the Caddyfile generator (`caddyfile_test.go` 1k lines).

## Where to look (question → path)

| Question | Path |
|---|---|
| Corrosion config, gossip addr, bootstrap | `internal/machine/machine.go:643-707` |
| Replicated schema | `internal/machine/store/schema.sql` |
| Own-row writers | `internal/machine/cluster.go:529-563`, `internal/machine/docker/controller.go:150-195` |
| Join gating on version vector | `internal/machine/cluster.go:369-483`, `store/store.go:63-117` |
| Membership UP/DOWN | `internal/machine/cluster/cluster.go:198-242`, `internal/corrosion/admin.go:215-266` |
| Subnet/name allocation race | `internal/machine/cluster/cluster.go:109-178` |
| WG peer rotation | `internal/machine/network/peer.go`, `wireguard_linux.go:281-440` |
| Peer reconfig on store change | `internal/machine/cluster.go:632-744` |
| Management IPv6 derivation | `internal/machine/network/ip.go:16-22` |
| DNS names & health filter | `internal/machine/dns/resolver.go:71-122`, `server.go:68-137` |
| Caddyfile generation/load | `internal/machine/caddyconfig/controller.go:80-201`, `caddyfile.go:21-65` |
| Deploy plan | `pkg/client/deploy/deploy.go:292-332`, `strategy.go:60-277` |
| Placement constraints | `pkg/client/deploy/scheduler/constraint.go`, `volume.go` |
| Health gate / rollback | `pkg/client/deploy/operation/container.go:39-68`, `156-220` |
| gRPC fan-out | `internal/machine/api/proxy/director.go`, `mapper.go` |
| SSH transport | `pkg/client/connector/ssh.go`, `sshcli.go`, `cmd/uncloudd/dialstdio.go` |
| Image push | `pkg/client/image.go:200-410` |
| Redlock | `pkg/distlock/locker.go`, `pkg/client/locker.go` |
| Corrosion v0→v1 migration | `internal/machine/corromigrate/migrate.go` |
| Test cluster | `internal/ucind/`, `test/e2e/cluster_test.go` |
| Design rationale | `misc/design.md`, README "How it works" (`README.md:141-278`) |

## What pilots BORROWED and how it maps

pilots refs are to `ARCHITECTURE.md` (rules 1-7, Contracts) and `apps/hostd/internal/*`.

| uncloud mechanism | uncloud path | pilots mechanism | pilots location |
|---|---|---|---|
| Corrosion as the only replicated store, one agent per host, local-replica reads | `store/store.go:25-32`, `machine.go:643-707` | rule 3 "State = Corrosion... every host reads its local replica" | `internal/state/corrosion/{service,rows,cache}.go`, `systemd/corrosion.service` |
| Corrosion runs beside the daemon, config generated by the daemon | `corroservice/docker.go` (container) | rule 2: run as a binary under systemd | `apps/hostd/systemd/corrosion.service` |
| Own-rows-only writing (containers/machines) — implicit | `cluster.go:529-563`, `docker/controller.go:150-195` | **Single-writer invariant** — explicit, reviewed | rule 3, AGENTS.md hard rule 1 |
| Machine info republished from local file every 60s | `cluster.go:32-34`, `485-518` | `hosts.last_seen` heartbeat; local `state.json` per machine + reconcile on restart | `hosts` table; "Persist + reconcile" section |
| Management IPv6 = `fdcc:` + pubkey[0:14] | `network/ip.go:16-22` | host address `fd cc + pubkey[0:14] /128` — same bytes | `.internal` section, `internal/mesh` |
| WG mesh, peers learned from the `machines` table subscription | `cluster.go:632-744` | mesh peers from `hosts` rows | `internal/mesh`, `cmd/hostd/meshup.go` |
| `.internal` DNS answered from a local subscription cache, healthy-only, TTL 0 | `dns/resolver.go`, `server.go:112` | `<machine>.internal` from "local Corrosion subscription cache... filtered to healthy", "Near-zero TTL, always" | `internal/dns` |
| Ingress config regenerated on every store change (Caddy) | `caddyconfig/controller.go:80-130` | router does a local Corrosion lookup per request | `internal/router` |
| Verify-reachability endpoint before publishing DNS | `pkg/client/dns.go:147-218` | wildcard DNS → all host IPs (no per-host verification) | rule 5 |
| Docker HEALTHCHECK semantics for gating | `operation/container.go:52-66` | health tagged union with `cmd` = Docker semantics | Corrosion schema comment |
| Health-gated rolling replace, kept-old rollback | `strategy.go`, `operation/container.go:156-220` | "health-gated deploy, kept-old rollback" | `POST /v1/services/:id/deploy`, `internal/services` |
| e2e drives the public client API against a multi-node throwaway cluster | `test/e2e/` + ucind | `scripts/e2e.mjs` drives public API only; 4-node laptop rig | AGENTS.md Testing |
| "Any machine is an entry point" | `1-connecting.md:126-127` | rule 1 "every host... serves the full API" | |
| Secrets resolved client-side (`secret://` + `x-command`) | 0.20 release notes, `pkg/client/compose/secret.go` | `secret://name` resolved client-side, then sealed by hostd | Environment and secrets section |

## Where pilots DIVERGES

1. **Explicit single-writer + deterministic ownership vs. uncloud's read-then-write uniqueness.** uncloud allocates subnets and checks name uniqueness from a snapshot of the replica on whichever machine the CLI hit (`cluster/cluster.go:109-178`), with the consensus TODO left open. pilots forbids this outright (rule 3 "no uniqueness constraints... Deterministic ownership `hash(key) mod live_hosts`"; "A cluster-wide subnet allocator would be a control plane"). **Improvement** for correctness under concurrent operations; **trade-off**: pilots depends on all hosts agreeing on `live_hosts`, which is itself an eventually-consistent read of `hosts.last_seen` — two hosts with different views of the live set compute different owners. uncloud sidesteps that by making the human the serialisation point (one CLI at a time).

2. **Reconciler (self-heal) vs. imperative-only.** uncloud: dead machine's containers stay dead; the author considers this acceptable for the target user and avoids any actor election. pilots: every survivor rescues `hash(machine_id) mod live_hosts == my_index` slices. **Improvement** in availability (it is table-stakes for a PaaS with scale-to-zero); **regression risk** that uncloud never has: double-rescue during a partition (two survivors with different live-sets both claim a machine), a rescue storm when a host flaps around the 30s threshold, and the "provably dead" judgment resting on gossip liveness alone. uncloud's `waitStoreSync` gate (`cluster.go:369-446`) shows the failure that motivates caution: acting on a partially replicated `machines` table locked machines out of the mesh (#155).

3. **S3 as truth vs. Docker as truth.** uncloud's durable state per machine is Docker's own state plus `machine.json`; wiping a disk loses that machine's containers and volumes. pilots: "wipe any host's disk; nothing is lost" (rule 4). **Improvement**, and it is what makes self-heal possible at all — uncloud *cannot* reschedule because the container's image may exist nowhere else and its volume certainly doesn't.

4. **Derived machine addresses vs. allocated subnets.** uncloud allocates `10.210.X.0/24` per machine and lets Docker IPAM hand out `.2-.254`; a container's IP therefore changes on every recreate and the machine subnet is a cluster-wide allocation. pilots derives `fdcd:<pubkey[0:12]>::<slot>` with no allocator, constant guest-side addresses, and NAT66 in the netns. **Improvement** for control-plane-freedom and snapshot portability; **trade-off**: pilots' scheme is far more intricate (two prefixes, per-veth filters, NAT in the namespace) and key rotation becomes a readdressing event. uncloud's scheme is debuggable with `ping 10.210.1.3` (the author sells this: "ping service containers by their service names", `1-overview.md:104-106`).

5. **Router in hostd vs. Caddy as a global container.** uncloud gets ACME, HTTP/3, x-caddy escape hatches for free but pays with per-machine cert stores (#31) and a config-reload pipeline. pilots terminates TLS in-process with one wildcard cert shared via S3 and holds connections during wake. **Improvement** for the wake-on-request product requirement (Caddy cannot "hold and restore"), and it dissolves #31 by construction; **regression**: everything Caddy gives for free (per-service custom config, plugins) must be reimplemented.

6. **Passive vs. membership-aware health.** uncloud relies on Caddy's `fail_duration` and DNS `healthy` flags computed by the *owning* machine — a dead owner never flips its rows to unhealthy (TODOs at `resolver.go:46`, `controller.go:133-135`). pilots' `hosts.last_seen` gives every host a membership signal to filter with. **Improvement**, but only if the router actually joins on it (check `internal/router` and `internal/dns` do).

7. **Firecracker vs. Docker.** Not a state-model difference, but it changes what "the container list" means: uncloud can treat Docker's inspect output as the row (`containers.container` JSON, `schema.sql:26-28`) because Docker is the durable per-host record. pilots' machine row must carry build ids, snapshot lineage, knobs — its rows are the *only* record. **Trade-off**: pilots' rows are load-bearing and schema migrations in Corrosion are, per the author, painful (#31, #172); uncloud's rows are a disposable cache of Docker.

8. **Desired state stored vs. not stored.** uncloud keeps no `services` table; the spec lives in container labels and the local `machine.db`. pilots has `services`/`releases` rows. **Improvement** for self-heal and rollback (you need the desired replica count somewhere the rescuer can read); **trade-off**: it re-introduces "who writes the services row" — pilots answers "host_id only", which means a service's row is owned by *a* host and must migrate on rescue.

9. **Trust model.** uncloud: SSH + unix group; mesh gRPC unauthenticated (`remote.go:364-366`); Corrosion API bearer token local only. pilots: bearer API keys hashed into an `api_keys` row (dashboard-host writer), multi-tenant jailer, sealed env with a fleet key. **Improvement** required by multi-tenancy; uncloud is single-tenant by design ("root on any machine is root on the entire cluster").

## Lessons pilots should COPY

1. **Gate store-dependent components on a version vector at join** (`cluster.go:369-483`, `store.go:63-117`): snapshot `crsql_db_versions` from the sponsoring host, pass it in the join request, block mesh-peer reconfiguration, DNS and self-heal until the local replica has caught up *and* `__corro_bookkeeping_gaps` is empty. pilots' self-heal is more dangerous than uncloud's peer reconfig if it runs on a half-replicated `hosts`/`machines` table — a fresh host could "rescue" machines that are alive. Put this in `internal/state/corrosion` before `internal/selfheal` starts.
2. **Never reconfigure the mesh from an empty peer list** (`cluster.go:641-667`, issue #155). Same guard in `internal/mesh`.
3. **Skip rows whose JSON columns are empty** (`store.go:196-201`): cr-sqlite materialises rows column-by-column. pilots' `rows.go`/`cache.go` should treat `machines.state == ""` or missing `host_id` as not-yet-replicated, not as a real row.
4. **Republish own rows on a timer, compare with `proto.Equal` first** (`cluster.go:529-563`) so a lost write self-corrects without churning the CRDT clock. pilots' `hosts` heartbeat already writes periodically; do the same for `machines` rows.
5. **Debounce Docker/FC events (100ms) plus a 30s full resync** (`docker/controller.go:19-23`, `72-148`) and upsert only when the serialised row changed (`store/container.go:401-411`) — otherwise every health tick becomes a gossip broadcast.
6. **Strip secrets before the row is written, and test it** (`store/container.go:422-437` + the #422 miss). pilots' `env_sealed` design is stronger, but keep the "no plaintext in a gossiped row" check as a test on `rows.go`.
7. **Local-first upstream ordering** (`caddyfile.go:115-119`) and `nearest.` DNS prefix (`server.go:87-99`): for pilots' N-replica router prefer the local replica before hopping the mesh.
8. **Resubscribe from last change id with bounded backoff** (`corrosion/client.go:148-256`), and treat a dead subscription as a controller failure that restarts the daemon rather than silently going stale (`cluster.go:673-678`).
9. **Cap QUIC gossip MTU below the minimum WG MTU** (`machine.go:679-685`); pilots runs Corrosion over the same WG mesh and Hetzner hosts may have different underlay MTUs.
10. **Pin and vendor the Corrosion build** (`Dockerfile:29`, `corroservice/docker.go:22-25`) and plan the dump-and-reseed path now (`corromigrate/migrate.go`); upstream v1 broke on-disk compatibility and the author lost months to it (#172).
11. **Publish the store version and RTTs in the fleet API** (`machine.go:1091-1115`) so `GET /v1/hosts` can show replication lag per actor — the first thing needed when a self-heal misfires.
12. **e2e: assert on every node's replica, not the entry node's** (`cluster_test.go:83-108`) — the 4-node laptop battery should check that `/v1/hosts` from each host agrees before running chaos steps.
13. **Verify public reachability before advertising a host in DNS** (`pkg/client/dns.go:147-218`): pilots' wildcard record lists *all* host IPs; a host whose :443 is firewalled will silently eat 1/N of traffic. Add a self-check to `host-bootstrap.sh` / `/v1/health`.

## What pilots should REJECT and why

1. **Read-then-write uniqueness on the replica** (`cluster/cluster.go:109-178`): pilots' rule 3 already forbids it; keep it forbidden even for "rare" paths like volume creation and custom-domain claims.
2. **"No rescheduling" as a product stance.** Correct for uncloud's audience, incompatible with scale-to-zero sandboxes whose host can die while suspended.
3. **Membership-blind derived views** (`resolver.go:46`, `controller.go:133-135`): pilots' router/DNS must join on `hosts.last_seen`.
4. **Storing the desired spec only in container labels**: rescue needs a `services` row.
5. **Per-machine cert stores** (#31): pilots' one-wildcard-via-S3 + HTTP-01-any-host is the right answer; do not adopt Caddy's default storage.
6. **Redlock across machines for coordination** (`pkg/distlock`): its own doc admits disjoint quorums under membership churn (`pkg/client/locker.go:42-46`); pilots' hash-mod-live-hosts ownership is simpler and needs no lease renewal loop. If pilots ever needs a lock (cert issuance for custom domains), use deterministic ownership + S3 conditional put, not Redlock.
7. **Plaintext tokens in the `cluster` kv** (`cluster/dns.go:445`): pilots seals; keep it that way for the Cloudflare DNS-01 token too (currently "certmagic + Cloudflare API token" — check it is in `/etc/pilots/config`, not a row).
8. **Full bootstrap list = every peer** (`machine.go:662-670`): fine at 3 machines, noted by the author as a TODO; pilots should bootstrap from a bounded random subset once the fleet exceeds a handful.

## Open questions

1. Does pilots' `internal/selfheal` gate on replication completeness at start-up (uncloud's `waitStoreSync` equivalent)? If not, a rebooted host with a stale replica can mis-count `live_hosts`.
2. Corrosion version: uncloud is on the unlabs-dev `2026.6.15` (upstream v1.0.0). Which version does pilots' `systemd/corrosion.service` pin, and does `state/corrosion/rows.go` assume v0 or v1 `db_version` semantics (they differ, per #172)?
3. uncloud gossips in plaintext inside WG (`machine.go:685`). Does pilots run Corrosion gossip only on the `fdcc::` mesh address, and does the netns egress filter guarantee guests can never reach `:51001`/hostd ports (pilots claims "Guests may only ever reach fdcd::/16")?
4. How does pilots handle the uncloud partial-row case for `machines` (state column present, `host_id` absent) in `cache.go`?
5. uncloud's author now wants "a self-hosted control center with observability" and "self-managed databases as first-class" (Substack, 2026-01). pilots' "tenant Postgres is a compose fragment" stance aligns; worth watching whether uncloud ends up adding a coordinator after all.
6. Show HN thread (https://news.ycombinator.com/item?id=43285730) returned 429 on direct fetch; the Algolia mirror yielded only the storage-related author comments quoted above. A second pass could capture his answers on Raft vs CRDT there specifically.

## Sources (web)

- https://news.ycombinator.com/item?id=46144570 — author comments (Dec 2025 HN thread): no rescheduling, reconciliation "locally during CLI execution", primary/standby idea.
- https://news.ycombinator.com/item?id=43285730 (via hn.algolia.com API) — Show HN, Mar 2025: volume replication stance, "Corrosion is only used for Uncloud's internal state".
- https://psviderski.substack.com/p/a-year-of-building-uncloud — Jan 2026: rollbacks as deal-breaker, 2026 roadmap.
- https://github.com/psviderski/uncloud/issues/31, /172, /119, /155, /65, /206, /369, /422 — design discussions quoted above.
- https://github.com/psviderski/uncloud/releases/tag/v0.20.0 — Corrosion v1 migration, server-side proxy resolution, unregistry port.
- https://github.com/psviderski/corrosion/releases — fork history (v0.2.2 RPi5 jemalloc fix; 2026.x tracks upstream v1).
- https://github.com/psviderski/uncloud-dns — Route53-backed managed DNS; domains purge after 30 days unrenewed, records after 2 days.
