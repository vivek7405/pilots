#!/usr/bin/env bash
# Run hostd on a single box, in the foreground.
#
# This is a SUBSET of the production path, never a parallel one:
# scripts/host-bootstrap.sh is not read and not edited by anything here, and
# the directory layout, the config key names and the binary path are the
# production ones. What a laptop cannot take from bootstrap is exactly what
# this adds: PILOT_STATE_BACKEND=sqlite (bootstrap writes corrosion
# unconditionally, and it is remote-only and Ubuntu-only besides), the golden
# image copied into place, and the binary run in the foreground rather than
# under a systemd unit whose Type=notify and Wants= are wrong for a box with
# no corrosion and no mesh.
#
# Ctrl-C is a detach, not an outage: hostd drains HTTP and deliberately leaves
# the machines running, and the next start re-adopts them.
#
# The object store has to be up first; see scripts/local-s3.sh. The full
# runbook is docs/local.md.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PREFIX="${PREFIX:-/opt/pilots}"
HOSTD="$PREFIX/bin/hostd"
CONFIG="${PILOT_CONFIG:-/etc/pilots/config}"
CHROOT_BASE="${PILOT_CHROOT_BASE:-/var/lib/pilots/jailer}"
KERNEL="${PILOT_KERNEL:-/opt/pilots/kernels/vmlinux-6.1.158/vmlinux.bin}"
GOLDEN_SRC="${PILOT_GOLDEN_SRC:-$REPO/scripts/rootfs/golden.ext4}"
GOLDEN_DST="${PILOT_TEMPLATE_ROOTFS:-/var/lib/pilots/templates/golden.ext4}"

[ "$(id -u)" = 0 ] || {
  cat >&2 <<'EOF'
local-host.sh must run as root. hostd needs it for three things, none of
which a user namespace can fake:

  - the jailer, which is passed --uid, --gid, --chroot-base-dir and --netns,
    then setns()es, chroots and setuids (internal/fc/boot.go)
  - veth pairs, taps and routes, created over netlink (internal/netns/setup.go)
  - nftables rules programmed per namespace (internal/netns/firewall.go)

  sudo scripts/local-host.sh
EOF
  exit 1; }

# A fleet host is bootstrapped by scripts/host-bootstrap.sh and is not this
# script's business. Refusing here is the hard constraint expressed in code:
# nothing local may touch what a production host reads.
if [ -f "$CONFIG" ] && grep -q '^PILOT_STATE_BACKEND=corrosion' "$CONFIG"; then
  echo "$CONFIG says PILOT_STATE_BACKEND=corrosion: this is a fleet host" >&2
  echo "bootstrapped by scripts/host-bootstrap.sh. local-host.sh is for a" >&2
  echo "single box and will not touch it." >&2
  exit 1
fi

# Not built here. This runs under sudo, where go (installed through mise under
# the user's HOME) is not on root's PATH, and apps/hostd/hostd is not
# gitignored so the output must not land in the tree either.
if [ ! -x "$HOSTD" ]; then
  cat >&2 <<EOF
$HOSTD is missing. Build and install it (as your user, then as root):

  (cd $REPO/apps/hostd && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/pilots-hostd ./cmd/hostd)
  sudo install -m0755 /tmp/pilots-hostd $HOSTD
EOF
  exit 1
fi

for tool in firecracker jailer; do
  [ -x "$PREFIX/bin/$tool" ] || {
    echo "$PREFIX/bin/$tool is missing; run scripts/fetch-firecracker.sh" >&2; exit 1; }
done
[ -f "$KERNEL" ] || {
  echo "$KERNEL is missing; run scripts/fetch-kernel.sh" >&2; exit 1; }

install -d -m0755 "$(dirname "$CONFIG")" "$(dirname "$GOLDEN_DST")" \
  "$CHROOT_BASE" /var/lib/pilots/machines /var/cache/pilots

# The jailer can create device nodes on a nodev filesystem but firecracker
# cannot open them, and the failure surfaces as an unrelated permission error.
# hostd catches this at the first create; catching it here means the script
# fails instead of the first machine.
if findmnt -T "$CHROOT_BASE" -no OPTIONS 2>/dev/null | grep -qw nodev; then
  echo "chroot base $CHROOT_BASE is on a filesystem mounted nodev; the jailer" >&2
  echo "can create device nodes there but firecracker cannot open them. Put it" >&2
  echo "on a normal disk filesystem, e.g. /var/lib/pilots/jailer." >&2
  exit 1
fi

# A machine's disk is served over NBD, so the module has to be loaded with
# enough devices for the machines this box will hold. A production host gets
# this from host-bootstrap.sh, which writes the modprobe config and loads it;
# a desktop kernel has the module built but not loaded, and the failure is a
# create that dies with "no network block devices exist" 30 seconds in.
if [ ! -e /dev/nbd0 ]; then
  echo "==> loading the nbd module (nbds_max=64)"
  modprobe nbd nbds_max=64
fi

if [ ! -f "$GOLDEN_SRC" ]; then
  echo "no golden rootfs at $GOLDEN_SRC. Build it:" >&2
  echo "  PATH=<docker shim>:\$PATH scripts/build-golden-rootfs.sh   # see docs/local.md" >&2
  exit 1
fi

# Two gigabytes, so it is compared before it is copied, exactly as
# host-bootstrap.sh compares before it scps. --reflink=auto makes the copy
# instant on btrfs or xfs and a plain copy anywhere else.
want="$(sha256sum "$GOLDEN_SRC" | cut -d' ' -f1)"
have="$(sha256sum "$GOLDEN_DST" 2>/dev/null | cut -d' ' -f1 || true)"
if [ "$want" = "$have" ]; then
  echo "==> golden rootfs already in place at $GOLDEN_DST"
else
  echo "==> copying the golden rootfs (2 GiB) to $GOLDEN_DST"
  cp --reflink=auto --sparse=always "$GOLDEN_SRC" "$GOLDEN_DST"
fi

# A local image is by definition built from this tree, so a difference from
# the COMMITTED pin is expected and is a warning, not the refusal production
# makes. What matters locally is that the guest agent inside the image is the
# one this tree builds, and there is a test for exactly that.
if [ -f "$REPO/scripts/rootfs/golden.ext4.sha256" ] &&
   ! ( cd "$REPO" && sha256sum -c scripts/rootfs/golden.ext4.sha256 >/dev/null 2>&1 ); then
  echo "    note: this image differs from the committed pin, which is normal for" >&2
  echo "    a locally built one. The check that matters here is:" >&2
  echo "      (cd apps/hostd && go test ./internal/build -run TestGoldenRootfsCarriesThisAgent)" >&2
fi

if [ -f "$CONFIG" ]; then
  echo "==> keeping the existing $CONFIG"
else
  echo "==> writing $CONFIG"
  umask 077
  cat > "$CONFIG" <<EOF
# Written by scripts/local-host.sh for a single box. Only what differs from
# hostd's defaults (internal/config/config.go) is here; kernel, firecracker,
# jailer, chroot base, template path, listen address and state DSN are the
# defaults, and are the same values a production host is given explicitly.
PILOT_STATE_BACKEND=sqlite
PILOT_WORKLOAD_DOMAIN=${PILOT_WORKLOAD_DOMAIN:-pilots.localhost}
PILOT_S3_ENDPOINT=${PILOT_S3_ENDPOINT:-http://127.0.0.1:9000}
PILOT_S3_BUCKET=${PILOT_S3_BUCKET:-pilots}
PILOT_S3_ACCESS_KEY=${PILOT_S3_ACCESS_KEY:-pilots}
PILOT_S3_SECRET_KEY=${PILOT_S3_SECRET_KEY:-pilots-secret}
# Fleet-wide secrets, generated once. A re-run keeps them: rotating the fleet
# key is a re-seal sweep of every sealed environment, and rotating the agent
# secret cuts every existing machine off from this host.
#
# Machine credentials are derived from this one.
PILOT_AGENT_TOKEN_SECRET=${PILOT_AGENT_TOKEN_SECRET:-$(head -c 32 /dev/urandom | base64 | tr -d '=/+')}
# Full base64, unlike the line above -- it is a 32-byte AES key, not an opaque
# string, so the padding matters.
PILOT_FLEET_KEY=${PILOT_FLEET_KEY:-$(head -c 32 /dev/urandom | base64)}
EOF
  chmod 0600 "$CONFIG"
  umask 022
fi

set -a
# shellcheck disable=SC1090  # the path is a knob, resolved at run time
. "$CONFIG"
set +a

domain="${PILOT_WORKLOAD_DOMAIN:-pilotrun.app}"
listen="${PILOT_LISTEN:-:8080}"
cat <<EOF

==> starting hostd. Ctrl-C drains HTTP and leaves the machines running.

    sudo $HOSTD bootstrap-key                       # mint an admin key
    curl http://api.${domain}${listen}/v1/health
    docs/local.md                                   # the rest of the runbook

EOF

exec "$HOSTD"
