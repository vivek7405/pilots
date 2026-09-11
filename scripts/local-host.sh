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
BUILDER_SRC="${PILOT_BUILDER_SRC:-$REPO/scripts/rootfs/builder.ext4}"
BUILDER_DST="${PILOT_BUILDER_ROOTFS:-/var/lib/pilots/templates/builder.ext4}"

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

# The guest agent, which is a different question from hostd's binary.
#
# hostd does not merely RUN the agent, it PACKS it into every image a build
# produces: internal/build's fixups read PILOT_GUEST_AGENT (default
# /opt/pilots/bin/guest-agent) and append it to the flattened tarball, because
# most base images carry no init at all and the agent becomes the image's
# /sbin/init. Without the file every build fails at "packing rootfs" with
# `open /opt/pilots/bin/guest-agent: no such file or directory` -- after the
# whole Dockerfile has been solved, which on a real application is minutes.
# host-bootstrap.sh installs it from the release tarball; nothing local did.
#
# Built rather than fetched, and rebuilt on every run, because the agent is
# version-tied to hostd: an image packed with a stale agent boots and answers
# nothing. `install` only when the bytes differ, so a re-run replaces nothing
# and the go build itself is a cache hit.
install_guest_agent() {
  local out="$PREFIX/bin/guest-agent" tmp
  # -trimpath and the same flags scripts/build-golden-rootfs.sh uses, so the
  # agent this installs is byte-identical to the one in the golden image and
  # TestGoldenRootfsCarriesThisAgent compares like with like.
  local build='CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w"'

  tmp="$(mktemp -t pilots-guest-agent-XXXXXX)"
  # go is installed per user (mise, asdf, /usr/local/go) far more often than
  # system-wide, and sudo resets PATH, so root usually cannot see it. Build as
  # the invoking user when that is the case: building as root with the user's
  # toolchain would leave root-owned entries in their module cache and break
  # their next build.
  if command -v go >/dev/null 2>&1; then
    ( cd "$REPO/apps/hostd" && eval "$build" -o "$tmp" ./cmd/guest-agent )
  elif [ -n "${SUDO_USER:-}" ] && runuser -l "$SUDO_USER" -c 'command -v go' >/dev/null 2>&1; then
    chown "$SUDO_USER" "$tmp"
    runuser -l "$SUDO_USER" -c \
      "cd '$REPO/apps/hostd' && $build -o '$tmp' ./cmd/guest-agent"
  fi
  if [ ! -s "$tmp" ]; then
    rm -f "$tmp"
    cat >&2 <<EOF
No go toolchain is reachable from here, so $out cannot be built.
hostd packs this binary into every image a build produces, so without it every
build fails at "packing rootfs". Build and install it (as your user, then as
root):

  (cd $REPO/apps/hostd && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /tmp/pilots-guest-agent ./cmd/guest-agent)
  sudo install -m0755 /tmp/pilots-guest-agent $out
EOF
    exit 1
  fi
  if cmp -s "$tmp" "$out"; then
    echo "==> guest agent already current at $out"
  else
    echo "==> installing the guest agent to $out"
    install -m0755 "$tmp" "$out"
  fi
  rm -f "$tmp"
}

for tool in firecracker jailer; do
  [ -x "$PREFIX/bin/$tool" ] || {
    echo "$PREFIX/bin/$tool is missing; run scripts/fetch-firecracker.sh" >&2; exit 1; }
done
[ -f "$KERNEL" ] || {
  echo "$KERNEL is missing; run scripts/fetch-kernel.sh" >&2; exit 1; }

install_guest_agent

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
#
# The test is whether the MODULE is loaded, not whether /dev/nbd0 is there: a
# stale or statically shipped device node makes the second question answer yes
# while the module is absent, and then nothing loads it and the create fails
# anyway -- the exact failure this block exists to prevent.
if [ ! -d /sys/module/nbd ]; then
  echo "==> loading the nbd module (nbds_max=64)"
  modprobe nbd nbds_max=64
else
  # Already loaded, possibly by something else and possibly at the module's own
  # default of 16. hostd's pool is 64 wide (nbd.DefaultMaxDevices) and reports
  # exhaustion against that number, so a box with fewer device nodes fails the
  # 17th create with "all 64 devices are in use" and names nothing. Say it here
  # rather than reload the module out from under whatever is using it.
  #
  # Read defensively: an unreadable or non-numeric nbds_max must warn, never
  # take the script down on "integer expression expected" under set -e.
  loaded="$(cat /sys/module/nbd/parameters/nbds_max 2>/dev/null || true)"
  if ! [ "${loaded:-0}" -ge 64 ] 2>/dev/null; then
    echo "note: nbd is already loaded with nbds_max=${loaded:-unknown}, and the 64" >&2
    echo "hostd's pool assumes may not be there. Past that many machines a" >&2
    echo "create fails with 'all 64 devices are in use'. Fix, when nothing" >&2
    echo "else is using nbd:" >&2
    echo "  sudo modprobe -r nbd && sudo modprobe nbd nbds_max=64" >&2
  fi
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

# The builder image, which is where a Dockerfile actually runs. Optional on a
# laptop: without it every other path works and only builds refuse, which is a
# better trade than refusing to stand the rig up at all.
if [ ! -f "$BUILDER_SRC" ]; then
  echo "==> no builder rootfs at $BUILDER_SRC; this host will refuse builds"
  echo "    build one with: VARIANT=builder scripts/build-golden-rootfs.sh"
elif [ "$(sha256sum "$BUILDER_SRC" | cut -d' ' -f1)" = "$(sha256sum "$BUILDER_DST" 2>/dev/null | cut -d' ' -f1 || true)" ]; then
  echo "==> builder rootfs already in place at $BUILDER_DST"
else
  echo "==> copying the builder rootfs (sparse) to $BUILDER_DST"
  cp --reflink=auto --sparse=always "$BUILDER_SRC" "$BUILDER_DST"
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

# The address hostd and buildkitd both reach the object store on.
#
# NOT loopback, and that is the whole point. buildkitd runs under
# `rootlesskit --net=slirp4netns --disable-host-loopback`, so inside it
# 127.0.0.1 is the daemon's OWN loopback and the host's is unreachable BY
# DESIGN -- slirp's 10.0.2.2 host alias is exactly what --disable-host-loopback
# turns off. hostd hands the S3 endpoint to buildkitd for the layer cache, so a
# loopback endpoint fails every build at `importing cache manifest from s3`
# with `dial tcp 127.0.0.1:9000: connect: connection refused`, minutes before
# anything is built. scripts/local-s3.sh listens on 0.0.0.0 precisely so a
# routable address works.
#
# Derived, never hardcoded: the source address the kernel would use to leave
# this box, then any global address if there is no default route (a laptop
# offline still has its bridges). A box with neither is told to say so itself
# rather than given a loopback that would fail later and elsewhere.
host_s3_address() {
  local addr
  addr="$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for(i=1;i<NF;i++) if($i=="src") print $(i+1); exit}')"
  [ -n "$addr" ] || addr="$(ip -4 -o addr show scope global 2>/dev/null |
    awk 'NR==1{split($4,a,"/"); print a[1]}')"
  if [ -z "$addr" ]; then
    echo "cannot work out a routable address for this host, and the object" >&2
    echo "store must not be reached over loopback (rootless buildkitd runs" >&2
    echo "with --disable-host-loopback). Set it yourself:" >&2
    echo "  sudo PILOT_S3_ENDPOINT=http://<this box>:9000 scripts/local-host.sh" >&2
    exit 1
  fi
  printf 'http://%s:9000' "$addr"
}

if [ -f "$CONFIG" ]; then
  echo "==> keeping the existing $CONFIG"
else
  echo "==> writing $CONFIG"
  # Resolved BEFORE the heredoc. Inside it this is a command substitution, and
  # an `exit 1` there ends the subshell and writes an empty endpoint rather
  # than stopping the script.
  s3_endpoint="${PILOT_S3_ENDPOINT:-$(host_s3_address)}"
  umask 077
  cat > "$CONFIG" <<EOF
# Written by scripts/local-host.sh for a single box. Only what differs from
# hostd's defaults (internal/config/config.go) is here; kernel, firecracker,
# jailer, chroot base, template path, listen address and state DSN are the
# defaults, and are the same values a production host is given explicitly.
PILOT_STATE_BACKEND=sqlite
PILOT_WORKLOAD_DOMAIN=${PILOT_WORKLOAD_DOMAIN:-pilots.localhost}
PILOT_S3_ENDPOINT=${s3_endpoint}
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
# The builder image. Builds run inside a per-org machine created from it, the
# same as on a fleet host, so this laptop runs no build daemon of its own.
# Without the image hostd refuses builds outright and deploys only stock
# images; build it with VARIANT=builder scripts/build-golden-rootfs.sh.
PILOT_BUILDER_ROOTFS=${PILOT_BUILDER_ROOTFS:-/var/lib/pilots/templates/builder.ext4}
EOF
  chmod 0600 "$CONFIG"
  umask 022
  # Deliberately left root-owned 0600. This script SOURCES it as root a few
  # lines below and hostd reads PILOT_JAILER and PILOT_FIRECRACKER out of it,
  # so a file the developer's own account can write is a root shell for
  # anything running as that account -- and it holds PILOT_FLEET_KEY and
  # PILOT_AGENT_TOKEN_SECRET besides. cmd/hostd's bootstrap-key tests used to
  # need it readable; they now point at their own temp file instead.
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
    curl http://api.${domain}:${listen##*:}/v1/health
    docs/local.md                                   # the rest of the runbook

EOF

exec "$HOSTD"
