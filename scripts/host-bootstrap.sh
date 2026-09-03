#!/usr/bin/env bash
# Turn a bare Ubuntu host into a pilots host.
#
#   scripts/host-bootstrap.sh <ip> [--peer <ip-of-any-existing-host>]
#
# Idempotent by presence-check at every step, so re-running it upgrades a host
# and never breaks one. The first host is bootstrapped with no --peer and
# forms a cluster of one; every host after that is given any existing host's
# mesh address and joins through it.
#
# This is the whole of "add a host = give an IP". There is no registration, no
# allocator, and nothing to update anywhere else: the new host mints its own
# mesh identity, derives its own address from it, and announces itself by
# writing its own row.
#
# ENVIRONMENT
#   PILOT_CORROSION_TOKEN      required -- the cluster's shared API secret
#   PILOT_AGENT_TOKEN_SECRET   required -- guest credentials are derived from it
#   PILOT_FLEET_KEY            required -- seals secret env values; keep it
#                              somewhere that outlives the fleet
#   PILOT_REQUIRE_REFLINK=1    THE FLEET SWITCH. Refuses to finish on a host
#                              that cannot share extents, and is also where the
#                              fleet-only requirements below are enforced.
#   PILOT_CPU_TEMPLATE         T2 / T2CL (Intel) or T2A (AMD). Required under
#                              PILOT_REQUIRE_REFLINK=1, and checked against the
#                              host's actual vendor before this exits.
#   PILOT_ACME_EMAIL           turns TLS on. Also turns on the port 80 and 443
#                              reachability probes.
#   PILOT_CLOUDFLARE_API_TOKEN DNS-01 credential for the wildcard certificate
#   PILOT_WORKLOAD_DOMAIN      default pilotrun.app
#   PILOT_S3_ENDPOINT / _REGION / _BUCKET / _ACCESS_KEY / _SECRET_KEY
#   PILOT_ROOTFS_TAG           download golden-<tag>.ext4.zst from the release
#                              when there is no local rootfs
#
# WHAT IT REFUSES TO FINISH ON, and why each is a refusal rather than a warning
#   * a golden.ext4 that is not the committed pin -- hosts bootstrapped on
#     different days would carry different base images
#   * a CPU vendor that disagrees with PILOT_CPU_TEMPLATE -- snapshots taken
#     here would be unrestorable everywhere else
#   * a corrosion config key corrosion does not read -- it ignores unknown keys
#     silently, so a typo runs on the default forever
#   * a host this machine cannot reach on 8080, 80 or 443 -- the wildcard
#     record lists every host, so a firewalled one eats 1/N of the traffic with
#     nothing reporting it
#   * no extent sharing, under PILOT_REQUIRE_REFLINK=1
#
# It also bounds every build to the pilot user's slice, so a build cannot
# starve the machines it shares a host with.
set -euo pipefail

# Pinned versions. Everything the fleet runs is a fixed artifact -- a host that
# quietly installed a different Firecracker would produce snapshots its peers
# could not restore.
# Matched to what the engine was developed and tested against. Firecracker
# makes no promise of snapshot compatibility across versions, and a fleet
# running a different one than its template was built on fails at restore --
# reporting a bad snapshot rather than a version mismatch.
FC_VERSION="1.16.1"
CORROSION_VERSION="1.0.0"
KERNEL_VERSION="6.1.158"
# The volume and build toolchain. Pinned for the same reason Firecracker is:
# a host that quietly installed a different JuiceFS would write chunks its
# peers cannot read, and a different BuildKit would produce images the fleet
# has never booted.
#
# Litestream is deliberately held at 0.3.x rather than the newest release --
# hostd writes its configuration file and drives `litestream restore` by flag,
# and both are contracts with a specific major line.
JUICEFS_VERSION="1.4.1"
LITESTREAM_VERSION="0.3.13"
BUILDKIT_VERSION="0.32.2"
ROOTLESSKIT_VERSION="3.1.0"

REPO="$(cd "$(dirname "$0")/.." && pwd)"
IP="${1:-}"
PEER=""
BUCKET="${PILOT_S3_BUCKET:-}"
S3_ENDPOINT="${PILOT_S3_ENDPOINT:-}"
S3_KEY="${PILOT_S3_ACCESS_KEY:-}"
S3_SECRET="${PILOT_S3_SECRET_KEY:-}"
CORROSION_TOKEN="${PILOT_CORROSION_TOKEN:-}"
AGENT_SECRET="${PILOT_AGENT_TOKEN_SECRET:-}"
FLEET_KEY="${PILOT_FLEET_KEY:-}"
DOMAIN="${PILOT_WORKLOAD_DOMAIN:-pilotrun.app}"
S3_REGION="${PILOT_S3_REGION:-}"
# The Firecracker CPU template this fleet is pinned to: T2 or T2CL on Intel,
# T2A on AMD. It normalises CPUID WITHIN a vendor, which is what lets a later
# host generation restore a snapshot an older one took. Unpinned, a fleet
# works until the day a new box has a different stepping, and then it fails at
# restore reporting a bad snapshot.
CPU_TEMPLATE="${PILOT_CPU_TEMPLATE:-}"
ACME_EMAIL="${PILOT_ACME_EMAIL:-}"
CF_TOKEN="${PILOT_CLOUDFLARE_API_TOKEN:-}"
SSH_OPTS="${SSH_OPTS:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null}"

shift || true
while [ $# -gt 0 ]; do
  case "$1" in
    --peer) PEER="${2:-}"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

[ -n "$IP" ] || { echo "usage: $0 <ip> [--peer <ip-of-any-existing-host>]" >&2; exit 2; }
[ -n "$CORROSION_TOKEN" ] || {
  echo "PILOT_CORROSION_TOKEN must be set: it is the cluster's shared API secret" >&2
  exit 2
}
[ -n "$AGENT_SECRET" ] || {
  echo "PILOT_AGENT_TOKEN_SECRET must be set: machine credentials are derived" >&2
  echo "from it, and a host that rescues a machine computes the same one." >&2
  exit 2
}
# The fleet key is the one piece of state whose durability is YOURS, not the
# platform's. Everything else lives in object storage and survives wiping every
# host; sealed environment values do not. It is deliberately not generated
# here: a script that minted one per host would produce a fleet where each host
# could only read the secrets it wrote, and the failure would not appear until
# the first rescue.
[ -n "$FLEET_KEY" ] || {
  echo "PILOT_FLEET_KEY must be set: it seals secret environment values before" >&2
  echo "they are written to a replicated row, and it must be the SAME on every" >&2
  echo "host. Keep it somewhere that outlives the fleet -- wipe every host and" >&2
  echo "sealed values are unrecoverable with object storage fully intact." >&2
  echo >&2
  echo "  export PILOT_FLEET_KEY=\$(openssl rand -base64 32)   # once, for the fleet" >&2
  exit 2
}
# PILOT_REQUIRE_REFLINK=1 is the "this is the fleet, not a laptop" switch, so
# it is where the fleet-only requirements are enforced. A production host
# without a pinned CPU template restores nothing a later host generation took,
# and the failure appears months later as an unrestorable snapshot rather than
# here as a missing variable.
if [ "${PILOT_REQUIRE_REFLINK:-0}" = 1 ] && [ -z "$CPU_TEMPLATE" ]; then
  echo "PILOT_CPU_TEMPLATE must be set on a fleet host: Firecracker memory" >&2
  echo "snapshots carry raw CPUID, and a template normalises it WITHIN a" >&2
  echo "vendor so a later host generation can restore what this one took." >&2
  echo "Unpinned, the fleet works until the next box has a different" >&2
  echo "stepping, and then it fails at restore reporting a bad snapshot." >&2
  echo >&2
  echo "  T2 or T2CL on Intel, T2A on AMD -- the SAME value on every host." >&2
  exit 2
fi

say() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
on_host() { ssh $SSH_OPTS "root@${IP}" "$@"; }

say "Bootstrapping ${IP}${PEER:+ (joining via ${PEER})}"

# What a joining host needs from its peer is the peer's KEY, not just its
# address. A mesh address is derived from a key one way -- there is no
# recovering the key from it, and without the key there is no tunnel, so
# gossip never reaches the peer and the hosts table never arrives.
PEER_MESH=""
PEER_BOOTSTRAP=""
if [ -n "$PEER" ]; then
  PEER_PUBKEY=$(ssh $SSH_OPTS "root@${PEER}" "wg pubkey < /var/lib/pilots/mesh.key")
  PEER_MESH=$(ssh $SSH_OPTS "root@${PEER}" "/opt/pilots/bin/hostd mesh-addr")
  PEER_BOOTSTRAP="${PEER_PUBKEY}@${PEER}:51820"
  echo "  peer ${PEER_MESH} via ${PEER}:51820"
fi

# ---------------------------------------------------------------------------
say "[1/10] Base packages, user and directories"
on_host bash -euo pipefail -s <<'REMOTE'
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq curl ca-certificates iproute2 iptables nftables \
  e2fsprogs wireguard-tools sqlite3 python3 \
  fuse3 uidmap slirp4netns fakeroot >/dev/null
# fuse3 is what a JuiceFS mount is; uidmap and slirp4netns are what rootless
# BuildKit needs to have a user namespace and a network without root; fakeroot
# is the fallback when this box's mke2fs cannot read a tarball, and it is the
# only way an unprivileged build can produce a rootfs whose files are
# root-owned.

# The nft BINARY is needed; the nftables SERVICE must not run. Its unit runs
# `nft -f /etc/nftables.conf`, and Ubuntu's stock file begins with
# `flush ruleset` -- so a reload, a package upgrade or a reboot silently
# deletes the tables hostd owns in the root namespace (internal/netns/wake.go,
# tenant.go) alongside the per-netns pilots-nat. Every machine on the host
# stops being reachable and nothing reports a cause.
systemctl disable --now nftables >/dev/null 2>&1 || true

# Clock sync is load-bearing, not hygiene: liveness compares a heartbeat
# stamped by one host's clock against another host's clock with a 30s
# threshold. A fresh box running tens of seconds off makes a live host
# "provably dead" -- and its machines get claimed while it serves them.
apt-get install -y -qq systemd-timesyncd >/dev/null 2>&1 || true
systemctl enable --now systemd-timesyncd >/dev/null 2>&1 || true
timedatectl set-ntp true

# The uid Firecracker is jailed to. Never root, and in the kvm group so it can
# open /dev/kvm from inside the jail.
id -u pilot-vm >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin pilot-vm
usermod -aG kvm pilot-vm

# The user builds run as. Never root: a build is an arbitrary user Dockerfile
# executing on a box that is also running other tenants' machines.
id -u pilot >/dev/null 2>&1 || useradd --system --create-home --shell /bin/bash pilot
grep -q "^pilot:" /etc/subuid || echo "pilot:100000:65536" >> /etc/subuid
grep -q "^pilot:" /etc/subgid || echo "pilot:100000:65536" >> /etc/subgid
# Without lingering, the user manager stops when nobody is logged in and
# buildkitd goes with it -- so the first build after a reboot fails against a
# socket that no longer exists.
loginctl enable-linger pilot >/dev/null 2>&1 || true

mkdir -p /var/lib/pilots/{machines,templates,corrosion} \
         /var/cache/pilots /run/pilots/corrosion /opt/pilots/{bin,kernels} /etc/pilots \
         /var/lib/pilot-volumes /mnt/pilot-volumes /var/cache/pilot-volumes \
         /etc/pilots/litestream
chmod 0700 /etc/pilots/litestream
REMOTE

# ---------------------------------------------------------------------------
say "[2/10] Kernel settings the engine depends on"
on_host bash -euo pipefail -s <<'REMOTE'
# The fault handler needs userfaultfd from an unprivileged process. Without
# this every restore fails at the handshake, reported as a permission error
# that names nothing useful.
# accept_ra=2 goes in BEFORE anything turns forwarding on. hostd enables
# net.ipv6.conf.all.forwarding so guest-to-guest traffic can cross from a
# machine's veth to the mesh -- and the kernel stops honouring router
# advertisements on a forwarding box, because a forwarding box is a router.
# A host that learned its IPv6 default route from an RA loses it the first
# time hostd starts, which looks like hostd breaking the host's networking.
cat >/etc/sysctl.d/60-pilots.conf <<'SYSCTL'
vm.unprivileged_userfaultfd = 1
# Ubuntu 23.10 and later ship AppArmor mediation of unprivileged user
# namespaces, on by default. Rootless buildkitd cannot create one, so it dies
# at startup with "fork/exec /proc/self/exe: operation not permitted" -- which
# names neither AppArmor nor user namespaces, and reaches a caller as a build
# that failed with exit status 1.
#
# The alternative is an AppArmor profile per binary. This host already runs
# untrusted code inside Firecracker microVMs under jailer and cgroups rather
# than relying on host-side mediation, and the build is the one place a user
# namespace is deliberately created, so the mediation is buying nothing here
# that the VM boundary does not already provide.
#
# The key does not exist on kernels without the patch, so this is written
# through a file that tolerates it being unknown.
kernel.apparmor_restrict_unprivileged_userns = 0
net.ipv6.conf.all.accept_ra = 2
net.ipv6.conf.default.accept_ra = 2
SYSCTL
# --system stops at the first unknown key on some releases; -e ignores keys
# this kernel does not have, which is the whole point of the line above.
sysctl -qe --system

# The block server needs network block devices. nbds_max is fixed at load
# time, so it goes in a modprobe conf rather than being set later.
cat >/etc/modules-load.d/pilots.conf <<'MODULES'
nbd
wireguard
MODULES
cat >/etc/modprobe.d/pilots-nbd.conf <<'MODPROBE'
options nbd nbds_max=64
MODPROBE
modprobe -r nbd 2>/dev/null || true
modprobe nbd nbds_max=64
modprobe wireguard
REMOTE

# ---------------------------------------------------------------------------
say "[3/10] Firecracker and jailer v${FC_VERSION}"
on_host bash -euo pipefail -s <<REMOTE
FC_DIR="/opt/firecracker/v${FC_VERSION}"
if [ -x "\$FC_DIR/firecracker" ] && "\$FC_DIR/firecracker" --version 2>&1 | grep -q "v${FC_VERSION}"; then
  echo "  already installed"
else
  mkdir -p "\$FC_DIR"
  ARCH=\$(uname -m)
  TMP=\$(mktemp -d); trap "rm -rf '\$TMP'" EXIT
  curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/v${FC_VERSION}/firecracker-v${FC_VERSION}-\${ARCH}.tgz" -o "\$TMP/fc.tgz"
  tar -xzf "\$TMP/fc.tgz" -C "\$TMP"
  install -m 0755 "\$(find "\$TMP" -name "firecracker-v${FC_VERSION}-\${ARCH}" -type f | head -1)" "\$FC_DIR/firecracker"
  install -m 0755 "\$(find "\$TMP" -name "jailer-v${FC_VERSION}-\${ARCH}" -type f | head -1)" "\$FC_DIR/jailer"
  echo "  installed v${FC_VERSION}"
fi
ln -sfn "\$FC_DIR/firecracker" /opt/pilots/bin/firecracker
ln -sfn "\$FC_DIR/jailer" /opt/pilots/bin/jailer
REMOTE

# ---------------------------------------------------------------------------
say "[4/10] Corrosion v${CORROSION_VERSION}"
on_host bash -euo pipefail -s <<REMOTE
# Pinned from day one, deliberately. The v0.x on-disk store cannot be upgraded
# in place, and starting on 1.0.0 is how we never write the migration.
# Checked against a marker rather than --version: the v1.0.0 binary reports
# itself as 0.2.0-beta.0, so a version grep never matches and every run
# re-downloads it.
if [ -x /opt/pilots/bin/corrosion ] && \
   [ "\$(cat /opt/pilots/bin/.corrosion-version 2>/dev/null)" = "${CORROSION_VERSION}" ]; then
  echo "  already installed"
else
  TMP=\$(mktemp -d); trap "rm -rf '\$TMP'" EXIT
  curl -fsSL "https://github.com/superfly/corrosion/releases/download/v${CORROSION_VERSION}/corrosion-x86_64-unknown-linux-gnu.tar.gz" -o "\$TMP/c.tgz"
  tar -xzf "\$TMP/c.tgz" -C "\$TMP"
  install -m 0755 "\$(find "\$TMP" -name corrosion -type f | head -1)" /opt/pilots/bin/corrosion
  echo "${CORROSION_VERSION}" > /opt/pilots/bin/.corrosion-version
  echo "  installed v${CORROSION_VERSION}"
fi
ln -sfn /opt/pilots/bin/corrosion /usr/local/bin/corrosion
REMOTE

# ---------------------------------------------------------------------------
say "[5/10] Guest kernel and golden rootfs"
# The rootfs is the one artifact the fleet takes from outside itself, so it is
# pinned. CI builds it at a tag and asserts it matches
# scripts/rootfs/golden.ext4.sha256; this refuses to ship anything else. An
# unpinned rootfs is not a cosmetic difference: hosts bootstrapped on different
# days would carry different base images, and the guest agent the image
# contains is version-tied to hostd.
#
# Note that scripts/build-golden-rootfs.sh REWRITES the .sha256 file as its
# last step, so a locally rebuilt image always matches its own hash. The pin
# that matters is the COMMITTED one -- `git status` on that file after a local
# build is the tell, and CI compares against the tagged tree, not the runner's.
if [ -z "${PILOT_ROOTFS_TAG:-}" ] || [ -f "${REPO}/scripts/rootfs/golden.ext4" ]; then
  :
else
  echo "  fetching golden-${PILOT_ROOTFS_TAG}.ext4.zst from the release"
  command -v zstd >/dev/null || { echo "  zstd is needed to unpack it" >&2; exit 1; }
  curl -fsSL -o "${REPO}/scripts/rootfs/golden.ext4.zst" \
    "https://github.com/vivek7405/pilots/releases/download/${PILOT_ROOTFS_TAG}/golden-${PILOT_ROOTFS_TAG}.ext4.zst"
  zstd -d -f "${REPO}/scripts/rootfs/golden.ext4.zst" -o "${REPO}/scripts/rootfs/golden.ext4"
  rm -f "${REPO}/scripts/rootfs/golden.ext4.zst"
fi
if [ -f "${REPO}/scripts/rootfs/golden.ext4" ]; then
  ( cd "$REPO" && sha256sum -c scripts/rootfs/golden.ext4.sha256 >/dev/null ) || {
    echo "  this golden.ext4 is not the pinned one." >&2
    echo "    Build it with scripts/build-golden-rootfs.sh at the tagged commit," >&2
    echo "    or set PILOT_ROOTFS_TAG=<tag> to download golden-<tag>.ext4.zst" >&2
    echo "    from the release. Refusing to ship an unpinned image." >&2
    exit 1
  }
  echo "  golden rootfs matches the pin"
fi
# Two gigabytes, so it is compared before it is copied. Without this every
# re-run ships it again -- which makes "re-running upgrades a host" cost
# minutes per host instead of seconds.
if [ -f "${REPO}/scripts/rootfs/golden.ext4" ]; then
  WANT=$(sha256sum "${REPO}/scripts/rootfs/golden.ext4" | cut -d' ' -f1)
  HAVE=$(on_host "sha256sum /var/lib/pilots/templates/golden.ext4 2>/dev/null | cut -d' ' -f1" || true)
  if [ "$WANT" = "$HAVE" ]; then
    echo "  golden rootfs already present"
  else
    echo "  copying the golden rootfs (2 GiB)"
    scp $SSH_OPTS -q "${REPO}/scripts/rootfs/golden.ext4" "root@${IP}:/var/lib/pilots/templates/golden.ext4"
  fi
else
  echo "  no local golden rootfs; the host will need one before creating machines"
fi
if [ -f "/opt/pilots/kernels/vmlinux-${KERNEL_VERSION}/vmlinux.bin" ]; then
  on_host "mkdir -p /opt/pilots/kernels/vmlinux-${KERNEL_VERSION}"
  scp $SSH_OPTS -q "/opt/pilots/kernels/vmlinux-${KERNEL_VERSION}/vmlinux.bin" \
    "root@${IP}:/opt/pilots/kernels/vmlinux-${KERNEL_VERSION}/vmlinux.bin"
fi

# ---------------------------------------------------------------------------
say "[5b/10] Volume and build toolchain"
# The heredoc is QUOTED, so nothing in it expands locally -- which is what
# keeps the systemd units below intact, backslash continuations and all. The
# versions travel as positional arguments instead.
on_host bash -euo pipefail -s \
  "$JUICEFS_VERSION" "$LITESTREAM_VERSION" "$BUILDKIT_VERSION" "$ROOTLESSKIT_VERSION" <<'REMOTE'
JUICEFS_VERSION="$1"; LITESTREAM_VERSION="$2"
BUILDKIT_VERSION="$3"; ROOTLESSKIT_VERSION="$4"

# Checked against a marker file rather than --version: several of these report
# a version string that does not match their release tag, so a version grep
# never matches and every re-run downloads the lot again.
install_from_tarball() {
  url="$1"; binary="$2"; dest="$3"; version="$4"
  if [ -x "$dest" ] && [ "$(cat "$dest.version" 2>/dev/null)" = "$version" ]; then
    echo "  $(basename "$dest") already at $version"
    return
  fi
  tmp=$(mktemp -d)
  curl -fsSL "$url" -o "$tmp/dl.tgz"
  tar -xzf "$tmp/dl.tgz" -C "$tmp"
  install -m 0755 "$(find "$tmp" -name "$binary" -type f | head -1)" "$dest"
  echo "$version" > "$dest.version"
  rm -rf "$tmp"
  echo "  installed $(basename "$dest") $version"
}

install_from_tarball \
  "https://github.com/juicedata/juicefs/releases/download/v${JUICEFS_VERSION}/juicefs-${JUICEFS_VERSION}-linux-amd64.tar.gz" \
  juicefs /opt/pilots/bin/juicefs "$JUICEFS_VERSION"

install_from_tarball \
  "https://github.com/benbjohnson/litestream/releases/download/v${LITESTREAM_VERSION}/litestream-v${LITESTREAM_VERSION}-linux-amd64.tar.gz" \
  litestream /opt/pilots/bin/litestream "$LITESTREAM_VERSION"

BUILDKIT_URL="https://github.com/moby/buildkit/releases/download/v${BUILDKIT_VERSION}/buildkit-v${BUILDKIT_VERSION}.linux-amd64.tar.gz"
for b in buildctl buildkitd buildkit-runc; do
  install_from_tarball "$BUILDKIT_URL" "$b" "/opt/pilots/bin/$b" "$BUILDKIT_VERSION"
done

# buildkitd finds its OCI worker by looking for a binary named `runc` on PATH.
# BuildKit ships it as buildkit-runc, so without this the daemon starts, finds
# no worker, and exits with "no worker found, rebuild the buildkit daemon?" --
# which reads like a broken download rather than a missing symlink.
ln -sf /opt/pilots/bin/buildkit-runc /opt/pilots/bin/runc

install_from_tarball \
  "https://github.com/rootless-containers/rootlesskit/releases/download/v${ROOTLESSKIT_VERSION}/rootlesskit-x86_64.tar.gz" \
  rootlesskit /opt/pilots/bin/rootlesskit "$ROOTLESSKIT_VERSION"

# Litestream replicates one volume's metadata database: a template unit, one
# instance per volume, started by hostd when it creates or takes over that
# volume.
#
# systemd's rather than a child of hostd, for the same reason the machines are
# systemd's: the durability it provides must not be a property of whether the
# daemon happens to be up. The stop timeout is load-bearing -- litestream
# flushes on a graceful shutdown, and a volume handover STOPS the unit to make
# that flush happen, so a short timeout turns the handover into a lost final
# upload.
cat >/etc/systemd/system/litestream@.service <<'UNIT'
[Unit]
Description=pilots volume metadata replication (%i)
After=network-online.target

[Service]
ExecStart=/opt/pilots/bin/litestream replicate -config /etc/pilots/litestream/%i.yml
Restart=always
RestartSec=2
TimeoutStopSec=60

[Install]
WantedBy=multi-user.target
UNIT

# Rootless BuildKit, as the unprivileged pilot user and never as root.
#
# --disable-host-loopback is load-bearing rather than hygiene: without it a
# Dockerfile can reach whatever is bound to the host's loopback, and on a
# pilots host that is the corrosion API and hostd's own control plane.
install -d -o pilot -g pilot /home/pilot/.config/systemd/user
cat >/home/pilot/.config/systemd/user/buildkitd.service <<'UNIT'
[Unit]
Description=pilots rootless buildkitd

[Service]
Environment=PATH=/opt/pilots/bin:/usr/local/bin:/usr/bin:/bin
ExecStart=/opt/pilots/bin/rootlesskit --net=slirp4netns --copy-up=/etc --copy-up=/run --disable-host-loopback /opt/pilots/bin/buildkitd --oci-worker-snapshotter=overlayfs
Restart=always
RestartSec=2
# A build is arbitrary user code running beside other tenants' machines. An
# unbounded one OOMs the host and takes every machine on it down with it.
MemoryMax=8G
CPUQuota=400%
TasksMax=4096

[Install]
WantedBy=default.target
UNIT
chown -R pilot:pilot /home/pilot/.config

# Build containment.
#
# It has to be user-<uid>.slice and cannot be a slice of our own naming:
# buildkitd is a USER unit, a unit inside the user manager cannot name a
# system slice as its parent, and everything that manager runs already lives
# under user.slice/user-<uid>.slice. Bounding that slice bounds buildkitd,
# rootlesskit, slirp4netns and every process a build spawns, in one place.
#
# Weights rather than caps, deliberately: a build should be able to use an
# idle host and should lose to a machine that is serving when the host is not
# idle. The per-unit MemoryMax, CPUQuota and TasksMax on buildkitd stay --
# a weight shares, a max stops, and an unbounded build still OOMs a host.
#
PILOT_UID=$(id -u pilot)
mkdir -p "/etc/systemd/system/user-${PILOT_UID}.slice.d"
cat >"/etc/systemd/system/user-${PILOT_UID}.slice.d/50-pilots-build.conf" <<'CONF'
[Slice]
# A build is arbitrary tenant code running beside other tenants' machines.
CPUWeight=20
MemoryHigh=8G
IOWeight=20
CONF

# cgroup v2 delegation, so the rootless daemon can put each build in its own
# slice. Without it buildkitd starts happily and every build fails on a cgroup
# write, which reads as a permission problem and is not one.
mkdir -p /etc/systemd/system/user@.service.d
cat >/etc/systemd/system/user@.service.d/delegate.conf <<'CONF'
[Service]
Delegate=cpu cpuset io memory pids
CONF

systemctl daemon-reload
runuser -u pilot -- env XDG_RUNTIME_DIR="/run/user/$PILOT_UID" \
  systemctl --user daemon-reload >/dev/null 2>&1 || true
if runuser -u pilot -- env XDG_RUNTIME_DIR="/run/user/$PILOT_UID" \
     systemctl --user enable --now buildkitd >/dev/null 2>&1; then
  echo "  buildkitd running"
else
  echo "  WARNING: buildkitd did not start; builds will fail on this host" >&2
fi
REMOTE

# ---------------------------------------------------------------------------
say "[6/10] hostd and the guest agent"
( cd "${REPO}/apps/hostd" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/pilots-hostd ./cmd/hostd )
scp $SSH_OPTS -q /tmp/pilots-hostd "root@${IP}:/opt/pilots/bin/hostd.new"
on_host "chmod 0755 /opt/pilots/bin/hostd.new && mv /opt/pilots/bin/hostd.new /opt/pilots/bin/hostd"
rm -f /tmp/pilots-hostd

# The agent is injected into every image a build produces. Without it a built
# machine boots and is unreachable: exec, the clock poke and the port proxy all
# go through it. Static, because the guest has no toolchain and no shared
# libraries we control -- and it may end up as the guest's PID 1.
( cd "${REPO}/apps/hostd" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o /tmp/pilots-guest-agent ./cmd/guest-agent )
scp $SSH_OPTS -q /tmp/pilots-guest-agent "root@${IP}:/opt/pilots/bin/guest-agent.new"
on_host "chmod 0755 /opt/pilots/bin/guest-agent.new && mv /opt/pilots/bin/guest-agent.new /opt/pilots/bin/guest-agent"
rm -f /tmp/pilots-guest-agent

# ---------------------------------------------------------------------------
say "[7/10] Mesh identity and host configuration"
# The identity is minted ON the host and never leaves it. Its mesh address is
# derived from the public half, so nothing has to hand one out -- which is what
# keeps adding a host from needing a registry.
on_host bash -euo pipefail -s <<'REMOTE'
if [ ! -s /var/lib/pilots/mesh.key ]; then
  umask 077
  wg genkey > /var/lib/pilots/mesh.key
fi
chmod 0600 /var/lib/pilots/mesh.key
REMOTE

PUBKEY=$(on_host "wg pubkey < /var/lib/pilots/mesh.key")
# Asked for, never recomputed. Deriving the address here as well would be a
# second implementation of the same rule, and the two disagreeing is silent:
# corrosion binds one address while the mesh answers on another, and gossip
# never arrives.
MESH_ADDR=$(on_host "/opt/pilots/bin/hostd mesh-addr")
HOST_ID="host-$(echo "$PUBKEY" | tr -d '=/+' | tr 'A-Z' 'a-z' | cut -c1-12)"
echo "  host id   ${HOST_ID}"
echo "  mesh addr ${MESH_ADDR}"

on_host bash -euo pipefail -s <<REMOTE
cat >/etc/pilots/config <<CONF
PILOT_HOST_ID=${HOST_ID}
PILOT_PUBLIC_IP=${IP}
PILOT_WORKLOAD_DOMAIN=${DOMAIN}
PILOT_STATE_BACKEND=corrosion
PILOT_MESH_ENABLED=1
PILOT_MESH_BOOTSTRAP=${PEER_BOOTSTRAP}
PILOT_CORROSION_ADDR=127.0.0.1:51002
PILOT_CORROSION_TOKEN=${CORROSION_TOKEN}
PILOT_AGENT_TOKEN_SECRET=${AGENT_SECRET}
PILOT_FLEET_KEY=${FLEET_KEY}
PILOT_S3_ENDPOINT=${S3_ENDPOINT}
PILOT_S3_BUCKET=${BUCKET}
PILOT_S3_ACCESS_KEY=${S3_KEY}
PILOT_S3_SECRET_KEY=${S3_SECRET}
PILOT_S3_REGION=${S3_REGION}
PILOT_CPU_TEMPLATE=${CPU_TEMPLATE}
PILOT_ACME_EMAIL=${ACME_EMAIL}
# The DNS-01 credential for the wildcard certificate. Every host holds it and
# every host manages the same names; the shared certificate storage lock is
# what turns N identical orders into one. Empty leaves the router HTTP-01-only
# with no wildcard, which serves custom domains and nothing else.
PILOT_CLOUDFLARE_API_TOKEN=${CF_TOKEN}
PILOT_TEMPLATE_ROOTFS=/var/lib/pilots/templates/golden.ext4
# buildkitd runs rootless as the pilot user, so its socket lives under THAT
# user's runtime directory. hostd is root and would derive /run/user/0, where
# there is no socket and the dial fails naming nothing about users.
#
# The uid is resolved by the remote shell at write time, not here: PILOT_UID
# is set inside an earlier heredoc whose shell has already exited, so a local
# expansion would silently produce /run/user//buildkit and fail identically.
# No backticks anywhere in this block -- the enclosing heredoc is unquoted, so
# they would run as a command substitution on the way out.
PILOT_BUILDKIT_SOCK=unix:///run/user/\$(id -u pilot)/buildkit/buildkitd.sock
PILOT_GUEST_AGENT=/opt/pilots/bin/guest-agent
PILOT_KERNEL=/opt/pilots/kernels/vmlinux-${KERNEL_VERSION}/vmlinux.bin
PILOT_FIRECRACKER=/opt/pilots/bin/firecracker
PILOT_JAILER=/opt/pilots/bin/jailer
CONF
chmod 0600 /etc/pilots/config
REMOTE

# ---------------------------------------------------------------------------
say "[8/10] Corrosion schema, config and units"
scp $SSH_OPTS -q "${REPO}/apps/hostd/internal/state/schema.sql" \
  "root@${IP}:/var/lib/pilots/corrosion/schema.sql"

# The units are files in the repo, not heredocs here. They were both for a
# while, and the two copies had already drifted: the repo's hostd unit carried
# Type=notify, Delegate=yes and LimitNOFILE while the one hosts actually ran
# carried none of them, and nothing installed the repo copy. Which unit a host
# is running is now answerable by reading one file.
scp $SSH_OPTS -q "${REPO}/apps/hostd/systemd/corrosion.service" \
  "root@${IP}:/etc/systemd/system/corrosion.service"
scp $SSH_OPTS -q "${REPO}/apps/hostd/systemd/hostd.service" \
  "root@${IP}:/etc/systemd/system/hostd.service"

# The schema file must be BYTE-IDENTICAL on every host: corrosion does not
# replicate DDL, so a host with a different one silently diverges.
BOOTSTRAP_LINE=""
[ -n "$PEER_MESH" ] && BOOTSTRAP_LINE="\"[${PEER_MESH}]:51001\""

on_host bash -euo pipefail -s <<REMOTE
cat >/var/lib/pilots/corrosion/config.toml <<CONF
[db]
path = "/var/lib/pilots/corrosion/store.db"
schema_paths = ["/var/lib/pilots/corrosion/schema.sql"]
# The default is -1048576: a 1 GiB SQLite page cache, sized for fly's fleet.
# This store holds thousands of rows, and that cache is the single largest
# term under the MemoryMax on the unit below. corrosion warns below 100 MiB,
# so 256 MiB is 2.5x the floor and still a quarter of the default.
cache_size_kib = -262144

[gossip]
addr = "[${MESH_ADDR}]:51001"
bootstrap = [${BOOTSTRAP_LINE}]
# The default, written down so the operator reading this file can see it: a
# mesh peer that vanished is dropped after 30s, the same window liveness uses
# to judge a host dead. The two agreeing is not a coincidence to be discovered
# later from a divergence.
idle_timeout_secs = 30
# Pinned from the smallest MTU any host could have. Left to discover it,
# QUIC overestimates across a heterogeneous underlay and gossip black-holes
# in a way that presents as the cluster flapping at random.
max_mtu = 1232
# The mesh already authenticates and encrypts every byte.
plaintext = true

[perf]
# How many unapplied changesets are buffered before corrosion starts DROPPING
# them. Left at the default on purpose: a re-seed replays this fleet's whole
# state at once, and a lower number would make the re-seed drop changes and
# then wait out anti-entropy to get them back -- turning a 60s convergence
# into an unbounded one.
processing_queue_len = 20000
# A bounded transaction keeps the local SQLite write lock short while a
# re-seed applies. An unbounded batch is how a busy apply loop starves every
# reader on the same database.
apply_queue_max_batch_size = 4000
# Checkpoint the WAL far more often than fly's 5 GiB default. This store is
# small; a WAL allowed to reach gigabytes is NVMe and page cache spent on
# nothing, and it is the term that grows when apply falls behind.
wal_threshold_mb = 512
sql_tx_timeout = 30

[telemetry.prometheus]
# corrosion's own counters for gossip and sync, on loopback only. This is the
# view of the state layer that a host has of ITSELF; cross-host replication
# lag is a separate signal and is not duplicated here.
bind_addr = "127.0.0.1:51003"

[api]
addr = "127.0.0.1:51002"

[api.authz]
bearer-token = "${CORROSION_TOKEN}"

[admin]
path = "/run/pilots/corrosion/admin.sock"
CONF

# Corrosion 1.0.0 SILENTLY IGNORES a key it does not recognise: Config::load
# deserialises through the `config` crate's try_deserialize and no struct
# carries deny_unknown_fields. So a typo -- processing_queue_length for
# processing_queue_len -- starts an agent that reports healthy and runs on the
# default forever. Nothing else in the system would ever say so, which is why
# the check lives here rather than in a comment asking for care.
#
# The allowlist is copied from crates/corro-types/src/config.rs at v1.0.0,
# serde aliases included. Re-check it when CORROSION_VERSION moves.
python3 - /var/lib/pilots/corrosion/config.toml <<'PY'
import sys, tomllib

ALLOWED = {
    "db": {"path", "schema_paths", "subscriptions_path", "cache_size_kib"},
    "api": {"bind_addr", "addr", "endpoint_name", "authorization", "authz", "pg"},
    "api.authz": {"bearer-token", "bearer"},
    "api.pg": {"bind_addr", "addr", "tls", "readonly"},
    "gossip": {"bind_addr", "addr", "external_addr", "client_addr", "bootstrap",
               "tls", "plaintext", "max_mtu", "idle_timeout_secs", "disable_gso",
               "member_id"},
    "gossip.tls": {"cert_file", "key_file", "ca_file", "insecure", "client"},
    "gossip.tls.client": {"cert_file", "key_file"},
    "perf": {"apply_channel_len", "changes_channel_len", "empties_channel_len",
             "to_send_channel_len", "notifications_channel_len",
             "schedule_channel_len", "clearbuf_channel_len", "bcast_channel_len",
             "foca_channel_len", "wal_threshold_mb", "sql_tx_timeout",
             "min_sync_backoff", "max_sync_backoff", "processing_queue_len",
             "apply_queue_timeout", "apply_queue_min_batch_size",
             "apply_queue_step_base", "apply_queue_max_batch_size",
             "apply_queue_batch_threshold_ratio"},
    "admin": {"uds_path", "path"},
    "telemetry": {"prometheus", "open_telemetry"},
    "telemetry.prometheus": {"bind_addr", "addr"},
    "log": {"format", "colors"},
}

path = sys.argv[1]
with open(path, "rb") as fh:
    doc = tomllib.load(fh)

bad = []


def walk(table, prefix):
    known = ALLOWED.get(prefix)
    if known is None:
        bad.append(f"[{prefix}] is not a corrosion config section")
        return
    for key, value in table.items():
        if isinstance(value, dict):
            walk(value, f"{prefix}.{key}" if prefix else key)
        elif key not in known:
            bad.append(f"{prefix}.{key} is not a corrosion config key")


for section, value in doc.items():
    if isinstance(value, dict):
        walk(value, section)
    else:
        bad.append(f"{section} is a top-level value; corrosion has none")

if bad:
    print("  corrosion config: NO -- corrosion would ignore these silently:",
          file=sys.stderr)
    for line in bad:
        print(f"    {line}", file=sys.stderr)
    print(f"    in {path}", file=sys.stderr)
    sys.exit(1)
print("  corrosion config: every key is one corrosion reads")
PY

# hostd brings the mesh up, and gossip rides the mesh -- so corrosion cannot
# reach its bootstrap peer until hostd has run. Ordering them the other way
# leaves a corrosion that sits retrying an address with no route to it.
cat >/etc/systemd/system/pilots-mesh.service <<'UNIT'
[Unit]
Description=pilots mesh interface
Before=corrosion.service
[Service]
Type=oneshot
RemainAfterExit=yes
EnvironmentFile=/etc/pilots/config
ExecStart=/opt/pilots/bin/hostd mesh-up
[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable pilots-mesh corrosion hostd >/dev/null

# Restarted explicitly rather than started with the enable flag: pilots-mesh
# is a oneshot with RemainAfterExit, so starting an already-active unit does
# nothing -- and a re-run would keep
# whatever peer the PREVIOUS run configured, with the new binary and the new
# config both ignored. That is silent: the unit reports active and the mesh
# reports up, while the peer that lets this host reach the fleet is missing.
systemctl restart pilots-mesh
systemctl restart corrosion
systemctl restart hostd
REMOTE

# ---------------------------------------------------------------------------
# A tunnel needs BOTH ends configured. The joining host was told about its
# peer, but the peer has never heard of it -- and it cannot, because the only
# channel that would tell it is the gossip that needs the tunnel.
#
# So the new host's row is seeded directly into the peer's state. That is not a
# parallel mechanism: the peer adds mesh peers from the hosts table like it
# does for everyone, gossip then flows both ways, and the row replicates to the
# rest of the fleet normally -- after which every host adds this one from its
# own copy of the table.
if [ -n "$PEER" ]; then
  say "[9/10] Introducing ${HOST_ID} to the fleet"
  PEER_TOKEN=$(ssh $SSH_OPTS "root@${PEER}" "grep PILOT_CORROSION_TOKEN /etc/pilots/config | cut -d= -f2")
  ssh $SSH_OPTS "root@${PEER}" "curl -sf --http2-prior-knowledge \
    -X POST http://127.0.0.1:51002/v1/transactions \
    -H 'Content-Type: application/json' \
    -H 'Authorization: Bearer ${PEER_TOKEN}' \
    -d '[{\"query\":\"INSERT INTO hosts (id,wg_addr,wg_pubkey,public_ip,last_seen) VALUES (?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET wg_addr=excluded.wg_addr, wg_pubkey=excluded.wg_pubkey, public_ip=excluded.public_ip\",\"params\":[\"${HOST_ID}\",\"${MESH_ADDR}\",\"${PUBKEY}\",\"${IP}\",0]}]'" >/dev/null \
    && echo "  introduced" || echo "  WARNING: could not introduce this host to ${PEER}"
fi

# ---------------------------------------------------------------------------
say "[10/10] Verifying"
# The heredoc below is quoted so nothing in it expands locally; that also means
# it cannot see this shell's environment, so the one value it needs travels as
# a positional argument.
on_host bash -euo pipefail -s "${PILOT_REQUIRE_REFLINK:-0}" <<'REMOTE'
# hostd serves only once corrosion has applied its schema, and it waits up to
# two minutes for that. A shorter window here fails a host that was fine.
for i in $(seq 180); do
  curl -sf http://127.0.0.1:8080/v1/health >/dev/null 2>&1 && break
  sleep 1
done
curl -sf http://127.0.0.1:8080/v1/health >/dev/null || { echo "  hostd is not serving" >&2; exit 1; }
echo "  hostd serving"
# `grep -c` exits 1 on a zero count, and under pipefail that fails the whole
# script -- on a host that is perfectly healthy and simply has no peers yet,
# which is ALWAYS true of the first host in a cluster.
PEERS=$(wg show pilots0 2>/dev/null | grep -c peer || true)
echo "  mesh peers: ${PEERS}"
systemctl is-active --quiet corrosion && echo "  corrosion running"

# hostd probes this at startup and reports it, so ask it rather than guessing
# from the filesystem type. A host without extent sharing still works and
# still passes every correctness test -- it is just several times slower at
# the two operations the product is named for, with nothing anywhere saying
# so. That is worth a loud line at the end of a bootstrap.
# mke2fs -d can read a tarball only if e2fsprogs was built with libarchive AND
# the shared library is there at run time. Neither is guaranteed, and a host
# missing it fails every build with an error that names neither. hostd probes
# the same thing at startup and falls back to unpacking under fakeroot; this
# says so out loud at bootstrap, because the fallback is slower and the
# operator should know which path this host is on.
PROBE=$(mktemp -d)
: > "$PROBE/empty"
tar -cf "$PROBE/probe.tar" -C "$PROBE" empty
if mke2fs -q -F -t ext4 -b 4096 -d "$PROBE/probe.tar" "$PROBE/probe.ext4" 2M >/dev/null 2>&1; then
  echo "  mke2fs tarball input: yes"
else
  echo "  mke2fs tarball input: NO -- e2fsprogs here has no libarchive support."
  echo "    Builds will unpack under fakeroot instead, which works and is slower."
fi
rm -rf "$PROBE"

# The rootless build daemon. A host whose buildkitd is not listening still
# serves machines perfectly well and cannot build anything, which is worth a
# line rather than a surprise on the first deploy.
PILOT_UID=$(id -u pilot 2>/dev/null || echo 0)
if [ -S "/run/user/${PILOT_UID}/buildkit/buildkitd.sock" ]; then
  echo "  buildkitd: listening"
else
  echo "  buildkitd: NOT listening -- builds will fail on this host" >&2
fi

# The containment a build actually runs under. Asked of systemd rather than
# read back from the drop-in file, because a drop-in that systemd never
# reloaded is a file that says the right thing and does nothing.
# One property per call: `systemctl show -p A -p B -p C` prints them in
# systemd's own internal order, not the requested one, so matching a joined
# string would report a correctly configured slice as unset whenever that
# order is not the one guessed here.
SLICE_CPU=$(systemctl show "user-${PILOT_UID}.slice" -p CPUWeight --value 2>/dev/null)
SLICE_MEM=$(systemctl show "user-${PILOT_UID}.slice" -p MemoryHigh --value 2>/dev/null)
SLICE_IO=$(systemctl show "user-${PILOT_UID}.slice" -p IOWeight --value 2>/dev/null)
if [ "$SLICE_CPU" = 20 ] && [ "$SLICE_MEM" = 8589934592 ] && [ "$SLICE_IO" = 20 ]; then
  echo "  build slice: CPUWeight=20 MemoryHigh=8G IOWeight=20"
else
  echo "  build slice: NO -- user-${PILOT_UID}.slice is CPUWeight=${SLICE_CPU:-unset}" >&2
  echo "    MemoryHigh=${SLICE_MEM:-unset} IOWeight=${SLICE_IO:-unset}." >&2
  echo "    A build here is arbitrary tenant code with no weight against the" >&2
  echo "    machines it shares the host with: one cargo build starves a" >&2
  echo "    serving neighbour and nothing reports why." >&2
fi

REFLINK=$(curl -sf http://127.0.0.1:8080/v1/health |
  python3 -c 'import sys,json;print(json.load(sys.stdin).get("reflink"))' 2>/dev/null || echo None)
if [ "$REFLINK" = True ]; then
  echo "  reflink: yes"
else
  echo "  reflink: NO -- $(findmnt -no FSTYPE -T /var/lib/pilots) cannot share extents." >&2
  echo "    Every machine image copy will be a real copy: create and checkpoint" >&2
  echo "    will run several times slower than the engine is designed for." >&2
  echo "    Put /var/lib/pilots on btrfs, or on XFS made with -m reflink=1." >&2
  # An `&&` chain here would be the last command in the script, so a false
  # test would exit non-zero under `set -e` and fail the bootstrap of a host
  # that is merely slow.
  if [ "${1:-0}" = 1 ]; then
    echo "    PILOT_REQUIRE_REFLINK=1 is set; refusing to finish." >&2
    exit 1
  fi
fi
REMOTE

# Reachability, checked FROM HERE and not from the host.
#
# This is the whole point of the check. Every probe above ran on the host and
# every one of them talks to 127.0.0.1, where a firewall rule is invisible. The
# wildcard A record lists every host IP, so a host whose port is closed to the
# world does not fail -- it silently eats 1/N of the fleet's traffic, and the
# only symptom is a fraction of requests timing out with no host reporting
# anything wrong. Adding a host to DNS before this passes is how that starts.
say "Checking ${IP} is reachable from here"
REACHED=""
if curl -sf -m 10 -o /dev/null "http://${IP}:8080/v1/health"; then
  REACHED="8080"
else
  echo "  unreachable: tcp 8080 on ${IP} does not answer /v1/health." >&2
  echo "    Check the Hetzner Robot firewall for ${IP}: 8080 must allow the" >&2
  echo "    fleet's host IPs and this machine's address." >&2
  exit 1
fi

# Port 80 and 443 only matter once ACME is configured. Without an ACME contact
# hostd never binds them, so probing would fail a host that is correct.
if [ -n "$ACME_EMAIL" ]; then
  CODE=$(curl -s -m 10 -o /dev/null -w '%{http_code}' "http://${IP}/" || echo 000)
  case "$CODE" in
    301|308) REACHED="${REACHED} 80" ;;
    *)
      echo "  unreachable: tcp 80 on ${IP} returned ${CODE}, want a redirect." >&2
      echo "    Port 80 is where the ACME HTTP-01 challenge lands. A host that" >&2
      echo "    does not answer it cannot obtain or renew a custom-domain" >&2
      echo "    certificate, and the failure shows up as an expiry months out." >&2
      echo "    Check the Hetzner Robot firewall for ${IP}." >&2
      exit 1
      ;;
  esac

  # Resolved to THIS host on purpose: the wildcard record points at every host
  # and this asks whether this one can serve the name.
  #
  # What is asserted is the HANDSHAKE, not the status code. api.<domain> has no
  # dispatch of its own yet (that is #30), so the router resolves it as a
  # machine name, finds no row, and answers 404 -- a 200 is not reachable from
  # here and never was. Curl's exit code is the honest signal: 0 means TLS
  # completed against a trusted wildcard, an SSL-class code means the port is
  # open but the certificate is not there yet, and anything else means nothing
  # answered at all.
  set +e
  curl -s -m 10 -o /dev/null \
    --resolve "api.${DOMAIN}:443:${IP}" "https://api.${DOMAIN}/v1/health"
  RC=$?
  set -e
  case "$RC" in
    0) REACHED="${REACHED} 443" ;;
    35|51|58|59|60|66|77|83|91)
      # The port is open and TLS was attempted, but the wildcard is not
      # serving yet. It is ordered asynchronously, so this is expected for the
      # first minutes of the first host and is never expected afterwards --
      # a warning rather than a refusal, because a bootstrap that fails here
      # would fail on a host that is entirely correct.
      echo "  cert: pending -- ${IP} accepts tcp 443 but the wildcard is not"
      echo "    serving yet (curl exit ${RC}). Expected on the first host for a"
      echo "    few minutes; on any later host it means"
      echo "    PILOT_CLOUDFLARE_API_TOKEN is wrong or the certificate storage"
      echo "    is not shared."
      ;;
    *)
      echo "  unreachable: tcp 443 on ${IP} did not answer (curl exit ${RC})." >&2
      echo "    Check the Hetzner Robot firewall for ${IP}." >&2
      exit 1
      ;;
  esac
fi
echo "  reachable: ${REACHED}"

# The CPU template and the CPU must agree. A T2A on Intel or a T2 on AMD is
# not a slow host, it is a host whose snapshots the rest of the fleet cannot
# restore -- and Firecracker reports that as a corrupt snapshot at restore
# time, on a machine belonging to a customer, months after the mistake.
if [ -n "$CPU_TEMPLATE" ]; then
  VENDOR=$(on_host "grep -m1 '^vendor_id' /proc/cpuinfo | awk '{print \$3}'")
  case "$CPU_TEMPLATE" in
    T2|T2CL) WANT_VENDOR=GenuineIntel ;;
    T2A)     WANT_VENDOR=AuthenticAMD ;;
    *)
      echo "  PILOT_CPU_TEMPLATE=${CPU_TEMPLATE} is not one of T2, T2CL, T2A." >&2
      exit 1
      ;;
  esac
  if [ "$VENDOR" != "$WANT_VENDOR" ]; then
    echo "  cpu template: NO -- ${CPU_TEMPLATE} needs ${WANT_VENDOR}, this host is ${VENDOR}." >&2
    echo "    A memory snapshot never restores across the Intel/AMD boundary." >&2
    echo "    Either this host does not belong in the fleet, or the fleet's" >&2
    echo "    PILOT_CPU_TEMPLATE is wrong. Refusing to finish." >&2
    exit 1
  fi
  echo "  cpu template: ${CPU_TEMPLATE} on ${VENDOR}"
fi

echo
echo "Host ${IP} is up."
echo "  host id:   ${HOST_ID}"
echo "  mesh addr: ${MESH_ADDR}"
echo
echo "Bootstrap the next host with:"
echo "  scripts/host-bootstrap.sh <ip> --peer ${IP}"
