# Incidents

Several rules in `ARCHITECTURE.md` exist because someone running this shape of
system hit the failure first and wrote it down: a per-host daemon, a gossiped
state store, and the claim that no host may depend on another. Reading other
operators' public post-mortems is how those rules landed before we paid for
them ourselves.

This directory is where ours go, so the next person gets the same head start.

## What goes here

Write an entry for every one of these. None of them is optional, and none of
them waits for someone to decide it was "big enough":

- **Anything a customer saw.** An error, a refused request, a timeout, or a
  latency above the SLO table below. A customer seeing it is the bar; whether
  they complained is not.
- **Every self-heal.** A host was judged dead and its machines were rescued.
  That is the fleet working, and it is also the single most dangerous code
  path we have — the one where two hosts can end up believing they own the
  same machine. Every firing gets written down whether or not it went well.
- **Every re-seed.** Any run of `scripts/corrosion-reseed.sh`, including the
  rehearsals in the cluster gate.
- **Every production sign-off run.** The numbers are the point of the run, and
  a run whose numbers are not written down did not happen.

## How

One file per event, named `YYYY-MM-DD-<slug>.md`, from `TEMPLATE.md`.

**Write it within 48 hours.** Not because of a policy, but because the
timeline is reconstructable for about that long and afterwards it is fiction.

**Numbers first.** "Create got slow" is not a finding. "Create p50 went from
430 ms to 2.1 s on host-c between 14:02 and 14:37 UTC" is. Every entry carries
the measurement table from the template, filled in, with the units.

**No blame, no hedging.** Name what happened and what caused it. If the cause
was a change we made, say which change.

**Follow-ups are owned and dated.** A follow-up with neither is a wish. If it
is worth writing, it is worth a name and a date; if it is not, delete it.

## The SLO table an entry is measured against

These are the metal SLOs the e2e battery enforces under `PILOTS_E2E_METAL=1`.
A latency above one of them, in production, is an incident.

| Operation | SLO |
|---|---|
| create | < 500 ms |
| wake | < 200 ms |
| checkpoint resume gap | < 500 ms |
| release restore (a replica from a release's build pair) | < 1 s |
| promote | < 1.5 s |
| re-seed convergence | < 60 s |

## The release procedure

The golden rootfs is the one artifact the fleet takes from outside itself, so
it is pinned end to end:

1. Tag the commit (`v<x.y.z>`). The CI `template` job runs only on tags.
2. That job builds `scripts/rootfs/golden.ext4` on the runner and asserts it
   matches the pin committed at the tag
   (`scripts/rootfs/golden.ext4.sha256`). A mismatch means the pin was not
   refreshed with the change that altered the image, and the tag is wrong.
3. It uploads `golden-<tag>.ext4.zst` and the pin to the GitHub release.
4. `scripts/host-bootstrap.sh` verifies the same pin before shipping a local
   `golden.ext4` to a host, and will download the release asset itself when
   `PILOT_ROOTFS_TAG` is set and no local file exists.

**The caveat, until an engine issue owns it:** a new rootfs does NOT rebuild
the memory template of a fleet that already has one. `loadTemplate` prefers
the manifest it already has (`apps/hostd/internal/machines/template.go`), so
after shipping a rootfs whose contents changed, the operator must retire the
existing template build so the fleet re-generates one from the new image.
Skipping this ships a rootfs that nothing boots from — every machine still
restores the old memory template, and nothing anywhere reports the mismatch.
