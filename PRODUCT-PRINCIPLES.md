# PRODUCT-PRINCIPLES.md — what pilots is, and what it refuses to become

The six non-negotiables. `ARCHITECTURE.md` says how the system is built and
`AGENTS.md` carries the bar a change is measured against; this file says what
the product **is for**, so that a design which satisfies both of those and
still misses the point can be caught.

Every one of these has already been argued for once. None of them is up for
quiet renegotiation in a pull request.

---

## 1. Simple on purpose. No central control plane.

Every host runs the identical stack and serves the full API from its local
replica. There is no scheduler tier, no managed database, no coordinator, no
"just one small service" that every request depends on.

A design that needs something in the middle has failed, however fast it is.
When two designs are equally good, the one with fewer moving parts wins.

**The worked example is e2b-infra**: a central API that is the only lifecycle
entry point — "the orchestrator cannot create a sandbox on its own initiative;
it does not even know the team" — placement decided centrally, **Nomad and
Consul as the orchestration tier** (Nomad schedules the per-node orchestrator
as a system job and is also how the API discovers nodes, with a documented
0–20 s discovery gap; Consul supplies service DNS), Redis as the routing
catalog, and 60+ runtime feature flags gating engine behaviour per sandbox.

That is four separate tiers — API, Nomad, Consul, Redis — each of which can be
down while every host is perfectly healthy and every sandbox is running. Here
there is one binary per host and a gossiped replica, and a host serves the full
API whether or not any other host is reachable.

## 2. Extremely cost efficient, with extremely fast wake.

Cost efficiency is why the platform exists. The lever is that a machine
nobody is using costs nothing to keep, and comes back fast enough that
keeping it running was never worth it.

Those two pull against each other and both must hold. A wake that is cheap
because it is slow has failed; a wake that is fast because the machine was
never really asleep has failed.

The wake is **L3** — a packet for a sleeping machine is what wakes it, counted
on the host that owns it. e2b resumes at L7 through its central API
(client-proxy to `ResumeSandbox`), and Fly cannot wake a private `.internal`
address at all without routing through its proxy. Waking from the packet
itself, with no proxy and nothing in the middle, is the deliberate
differentiator and it is downstream of principle 1.

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
Fly spent three years adding migration to host-pinned volumes; the cost of
getting this wrong is measured in years, and it is avoided by never pinning
anything to a host in the first place.

The harder half is a host that dies **while machines are running on it**. A
paused e2b sandbox can be resumed elsewhere, but a running one dies with its
node: there is no dead-host recovery, and an orchestrator restart kills what
it was running rather than re-adopting it. Pilots recovers a machine whose
host is gone, from object storage, onto a survivor that chose itself with
nobody coordinating — and its own daemon restarts without stopping a single
running machine. That is the property this principle is really about.

## 5. One storage model: S3 is the volume, the host disk is only a cache.

The machine root and the volume are one S3-backed thing. Truth lives in the
bucket; local NVMe is a read-through cache that can be wiped at any moment
without losing anything. Wipe any host's disk and nothing is lost.

Not two models. Not a local rootfs copy beside a network volume. Not a
host-pinned disk. The test is the one Fly's own team stated after replacing
their host-pinned tier: the durable state of a machine should be a URL.

Durability is **stated, not implied**: a volume is per-write durable, and the
machine root has a published, measured RPO. A window nobody publishes is not
a guarantee, it is a hope.

One model also means one place truth lives. e2b splits it across Postgres for
durable rows, Redis for the running set and routing, and object storage for
the artifacts — three stores that can disagree. Here the bucket is the truth
and everything on a host is derived from it.

## 6. A 2-in-1 sandbox and PaaS, on one primitive.

A sandbox for an agent and a durable production service are the same
`machine` with different lifecycle knobs. `promote` turns one into the other
without changing its URL.

The comparison is fly + sprites: two products built separately and combined
after the fact, still shipping two CLIs (`fly` and `sprite`). e2b is the other
half of the gap — a sandbox product with no PaaS face at all: no services, no
volumes, no permanent URLs (its routing entries carry a TTL and its catalog is
bounded in hours). Pilots is one product, one primitive, one CLI, designed as
the 2-in-1 from the start. Any
change that starts to split the two faces apart — a second command surface, a
second lifecycle, a capability only one of them can reach — is moving toward
the shape this product exists to avoid.

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
how pilots should be shaped. `docs/prior-art/` carries the sourced notes, each
ending in what to copy and what to reject.

## How to use this

Measure a design against this list **before** proposing it, not after
implementing it. Reject anything that:

- adds a per-suspended-machine cost on the host,
- pins durable state to a particular host,
- introduces a coordinator any request path depends on,
- keeps two storage models where one would do,
- or splits the sandbox and the PaaS into two products.

`AGENTS.md` bar items 6 and 7 are the enforceable form of principles 2–5, and
a pull request argues with the bar there. This file is why those bars exist.
