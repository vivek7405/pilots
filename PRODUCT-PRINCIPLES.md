# PRODUCT-PRINCIPLES.md — what pilots is, and what it refuses to become

The six non-negotiables. `ARCHITECTURE.md` says how the system is built and
`AGENTS.md` carries the bar a change is measured against; this file says what
the product **is for**, so that a design which satisfies both of those and
still misses the point can be caught.

Every one of these has already been argued for once. None of them is up for
quiet renegotiation in a pull request.

**On the evidence below.** Each principle names the prior art that motivated
it and points at `docs/prior-art/` for the sourced version. The pointer is
deliberate: those notes carry a URL or a `path:line` on every claim and are
kept current, and a second copy of a comparison rots exactly the way a second
copy of a contract does. Facts about other products belong there. What belongs
here is what pilots does about them.

---

## 1. Simple on purpose. No central control plane.

Every host runs the identical stack and serves the full API from its local
replica. There is no scheduler tier, no managed database, no coordinator, no
"just one small service" that every request depends on.

A design that needs something in the middle has failed, however fast it is.
When two designs are equally good, the one with fewer moving parts wins.

The same rule decides what it takes to **run** pilots. Adding a host is
`scripts/host-bootstrap.sh <ip>` against a bare-metal box and an S3 endpoint.
Every dependency on a cloud-managed appliance — a load balancer, a managed
filer, a hosted queue — is a machine somebody cannot self-host on, and is
rejected for that reason alone.

**The worked example is e2b-infra**: a central API that is the only lifecycle
entry point, with placement decided centrally, Nomad and Consul as the
orchestration tier, Redis as the routing catalog, and feature flags gating
engine behaviour per sandbox. That is four tiers that can each be down while
every host is healthy and every sandbox is running. Its self-hosting floor is
a cloud account: `iac/` ships an AWS provider and a GCP provider and nothing
else. Here there is one binary per host and a gossiped replica, and a host
serves the full API whether or not any other host is reachable.
(`docs/prior-art/e2b-infra.md`, "Central control plane" and the REJECT list.)

## 2. Extremely cost efficient, with extremely fast wake.

Cost efficiency is why the platform exists. The lever is that a machine
nobody is using costs nothing to keep, and comes back fast enough that
keeping it running was never worth it.

Those two pull against each other and both must hold. A wake that is cheap
because it is slow has failed; a wake that is fast because the machine was
never really asleep has failed.

The wake is **L3** — a packet for a sleeping machine is what wakes it, counted
on the host that owns it. Waking from the packet itself, with no proxy and
nothing in the middle, is the deliberate differentiator, and it is downstream
of principle 1: the alternatives wake at L7 through a central tier because
they have one. (`docs/prior-art/e2b-infra.md` REJECT "L7-only, API-mediated
wake-on-request"; `docs/prior-art/fly-io.md` §on fly-proxy.)

**The wake is at par with Fly's or better, and that is measured, not
asserted.** Fly Machines are the established suspend-on-idle platform, so
they are the bar, and the comparison is the same image on both: the webjs
website, 512 MB, suspend on idle, one request sent only after the platform
itself reports the machine `suspended`, and the number is the server wait
(time to first byte minus the connect and TLS handshake, so the network path
to Fly's region counts against neither side). Measured 2026-09-17, five
rounds each, with the method in the PR that shipped the S3-backed root:

| | pilots (laptop host) | Fly (`shared-cpu-1x`, `sin`) |
|---|---|---|
| wake, median | **0.58 s** | 4.4 s |
| wake, best / worst | 0.46 s / 0.62 s | 3.1 s / 9.6 s |

A change that moves pilots' figure toward Fly's is a regression, whatever
else it improves; the e2e battery's wake and resume SLOs are the floor and
this table is what they exist to protect.

## 3. A suspended machine occupies next to nothing on its host.

No process, no veth, no reserved memory, no per-machine object left warm on
the off chance. The wake path's host-side cost is **one** interface per host
for the whole address block, never a thing per sleeping machine.

This is the principle most easily lost by accident, because every "just keep
a little state around" optimisation looks free in isolation and is paid for
by every idle machine on the host.

## 4. Cross-host is the default case, not an edge case.

Wake, restore, rescue, volumes and checkpoints must all work on a host other
than the one the machine last ran on, within the CPU-vendor pool that
`ARCHITECTURE.md` rule 6 defines.

A feature that works only where the machine happens to be is not finished.
Fly's own account of retrofitting this is the warning: "It took 3 years to get
workload migration right with attached storage, and it's still not 'easy'."
Three years, for a team that is very good at this, because the pinning came
first. The cost of getting it wrong is measured in years and it is avoided by
never pinning anything to a host in the first place.

The harder half is a host that dies **while machines are running on it**.
Resuming a *paused* workload elsewhere is the easy direction and several
platforms do it; recovering a *running* one whose host is gone is the property
this principle is really about. Pilots rebuilds it from object storage onto a
survivor that chose itself with nobody coordinating, and its own daemon
restarts without stopping a single running machine.
(`docs/prior-art/INDEX.md` scorecard rows "Cross-host recreate from object
storage" and "Self-heal on host death".)

## 5. One storage model: S3 is the volume, the host disk is only a cache.

The machine root and the volume are one S3-backed thing. Truth lives in the
bucket; local NVMe is a read-through cache that can be wiped at any moment
without losing anything. Wipe any host's disk and nothing is lost.

Not two models. Not a local rootfs copy beside a network volume. Not a
host-pinned disk. The test is the one Fly's team stated for Sprites, their
object-storage-backed sandbox product: the durable state of a Sprite is
simply a URL.

Durability is **stated, not implied**: a volume is per-write durable, and the
machine root has a published, measured RPO. A window nobody publishes is not
a guarantee, it is a hope.

The counter-example is sharp because it is so nearly right. e2b already keeps
build artifacts in object storage, and still ends up running **three** storage
models: object storage for templates and snapshots, a managed cloud NFS filer
for user volumes, and a local NVMe overlay for the root whose durable form is
the last snapshot it managed to upload. Three places truth can live, two of
which can disagree. Splitting truth is the easy mistake, and one model is the
whole of the fix. (`docs/prior-art/e2b-infra.md` §9 and the REJECT list;
`docs/prior-art/sprites-dev.md` §4.)

## 6. A 2-in-1 sandbox and PaaS, on one primitive.

A sandbox for an agent and a durable production service are the same
`machine` with different lifecycle knobs. `promote` turns one into the other
without changing its URL.

The comparison is fly + sprites: two products built separately and combined
after the fact, still shipping two CLIs (`fly` and `sprite`). Sprites run on
top of Fly Machines rather than replacing them, which is what a second product
looks like from the inside. e2b is the other half of the gap, a sandbox
product with no PaaS face: no services, no health-gated rollout, no custom
domains, and a routing entry that expires with the sandbox's own lifetime,
where a permanent URL is an architecture rule here.

Pilots is one product, one primitive, one CLI, designed as the 2-in-1 from the
start. Any change that starts to split the two faces apart — a second command
surface, a second lifecycle, a capability only one of them can reach — is
moving toward the shape this product exists to avoid.

---

## A note on e2b-infra, which these principles keep citing

e2b-infra is cited above as the counter-example four times, and that is only
half of what it is. It is **open source and cloned locally**, which makes it
the one place the hard half of this product already exists as readable code:
the userfaultfd handler, the chained memfile/diff block layout, the in-process
NBD server, netns and slot addressing, guest provisioning, pre-pause hygiene.
When the question is *how does this mechanism work*, read it before guessing —
that is a rule in `AGENTS.md`, and the alternative has already cost time here.

The line is clean and worth holding: **take mechanics, never architecture.**
Below their gRPC boundary is an engine worth learning from. Above it is the
control plane these principles exist to replace. Something can be excellent
code and still be a shape this product refuses; both halves of that are true
of e2b at once, and neither cancels the other.

Same rule for fly and sprites, which have the advantage of being further along
and the disadvantage of being closed: their published writing is evidence about
mechanics and about what the tradeoffs cost in production, not a template for
how pilots should be shaped.

## How to use this

Measure a design against this list **before** proposing it, not after
implementing it. Reject anything that:

- adds a per-suspended-machine cost on the host,
- pins durable state to a particular host,
- introduces a coordinator any request path depends on,
- keeps two storage models where one would do,
- needs a cloud-managed appliance to stand a host up,
- or splits the sandbox and the PaaS into two products.

`AGENTS.md` bar items 6 and 7 are the enforceable form of principles 2–5, and
a pull request argues with the bar there. This file is why those bars exist.
