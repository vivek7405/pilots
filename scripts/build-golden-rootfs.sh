#!/usr/bin/env bash
# Build the golden ext4 rootfs that every machine is created from.
#
# Route: docker export -> fakeroot -> mke2fs -d. This needs NO root, no loop
# mount, and no debootstrap, so it runs identically on an Arch laptop and on
# an Ubuntu CI runner.
#
# The single most important detail: the tar extract and the mke2fs must happen
# inside ONE fakeroot session. fakeroot keeps its uid/gid map in memory per
# session, so splitting them silently loses every ownership and setuid bit --
# producing a rootfs where nothing is root-owned and sudo is broken, which
# then fails at runtime rather than at build time.
set -euo pipefail

# die stops with a reason. Used where a failure would otherwise carry on with
# an empty path, which `set -e` does not catch inside a function called in a
# conditional: errexit is suppressed for the whole call.
die() { printf 'build-golden-rootfs: %s\n' "$*" >&2; exit 1; }

cd "$(dirname "$0")/.."

# Two variants come out of this one script, because everything below the
# Dockerfile -- the reproducibility probe, the single fakeroot session, the
# resolv.conf and /sbin/init fixups -- is identical for both and is the part
# that is expensive to get right.
#
#   golden   the rootfs every machine is created from
#   builder  that image plus a rootful BuildKit daemon, which is what a
#            per-org builder machine runs so that a customer `RUN` step
#            executes behind KVM instead of on the host
#
# They share ONE docker context (scripts/rootfs) so that eth0.network and
# guest-agent.service are not duplicated; the variant selects the Dockerfile
# within it.
VARIANT="${VARIANT:-golden}"
case "$VARIANT" in
  golden)
    DOCKERFILE="${DOCKERFILE:-scripts/rootfs/Dockerfile}"
    SIZE_MB="${SIZE_MB:-2048}"
    OUT="${OUT:-scripts/rootfs/golden.ext4}"
    IMAGE="${IMAGE:-pilots-golden-rootfs}"
    # These two literals are what make the image byte-reproducible; see below.
    FS_UUID="${FS_UUID:-6f696c70-7473-4000-8000-676f6c64656e}"
    FS_HASH_SEED="${FS_HASH_SEED:-70696c6f-7473-4000-8000-736565646564}"
    ;;
  builder)
    DOCKERFILE="${DOCKERFILE:-scripts/rootfs/Dockerfile.builder}"
    # 32 GiB sparse. A builder holds BuildKit's snapshotter store for every
    # image one org builds from, which the golden image's 2 GiB cannot fit.
    # It costs nothing until written: the actual size is reported at the end.
    SIZE_MB="${SIZE_MB:-32768}"
    OUT="${OUT:-scripts/rootfs/builder.ext4}"
    IMAGE="${IMAGE:-pilots-builder-rootfs}"
    # Distinct from golden's, so the two filesystems are never confusable by
    # UUID on a host that carries both.
    FS_UUID="${FS_UUID:-6275696c-7473-4000-8000-6275696c6465}"
    FS_HASH_SEED="${FS_HASH_SEED:-6275696c-7473-4000-8000-736565646564}"
    ;;
  *)
    echo "unknown VARIANT '$VARIANT' (expected: golden, builder)" >&2
    exit 2
    ;;
esac

# Three knobs aimed at making the PACK byte-reproducible, so that packing one
# filesystem twice is idempotent and the pin in golden.ext4.sha256 can mean
# something.
#
# What they do NOT make reproducible is the image being packed. The Dockerfile
# reaches the network in three places -- `FROM ubuntu:24.04` is a tag rather
# than a digest, `apt-get install` takes whatever the archive holds today, and
# the Node step pipes deb.nodesource.com's setup script into bash -- so two
# builds separated by an upstream change produce different filesystems, and no
# amount of clamping here can alter that.
#
# So the pin means "this ext4 is the one built from this tree at this commit,
# on a machine whose upstream state matched". It is verified against the
# artifact published at a tag, which is why host-bootstrap.sh offers to
# download that artifact rather than telling everybody to rebuild. Making the
# base system reproducible across time as well is a real and separate piece of
# work: pin the digest, point apt at a snapshot archive, and install Node from
# a checksummed tarball.
#
#   -U                 mke2fs otherwise picks a random filesystem UUID
#   -E hash_seed=      the directory hash seed is otherwise random, even with -U
#   SOURCE_DATE_EPOCH  the superblock and inodes otherwise carry this run's clock
#
# READ THIS BEFORE TRUSTING THE RESULT. The third one depends on the mke2fs
# doing the work, and older builds ignore it entirely: 1.47.4 honours it,
# 1.47.0 -- which is what Ubuntu 24.04 and therefore GitHub's runners ship --
# does not, so on those the image still varies run to run. Measured in a
# ubuntu:24.04 container, not inferred from a changelog.
#
# So the script PROBES rather than assumes, below, and says so when its
# toolchain cannot deliver. A build that quietly stopped being reproducible
# would put us back where this started: a pin that asserts nothing while
# host-bootstrap.sh still hard-fails anyone whose image does not match it.
#
# So the pack runs inside a PINNED container with a known e2fsprogs, which is
# the only thing that gets reproducibility ACROSS machines -- no combination of
# flags on the host can, because the flags are honoured by some builds and
# ignored by others. `PACK_IMAGE` below is that pin, and it is the reason the
# hash in `*.sha256` means the same bytes on a laptop, on a runner and on a
# host, rather than the same bytes on one of them.
#
# `PACK_LOCAL=1` runs the pack on the host toolchain instead, for somebody
# without Docker. The probe below then still says whether the result is
# meaningful, which is what it was always for.
: "${SOURCE_DATE_EPOCH:=1700000000}"
export SOURCE_DATE_EPOCH

# The pinned packing toolchain. debian:trixie-slim carries e2fsprogs 1.47.2 or
# newer, which honours SOURCE_DATE_EPOCH; bookworm ships 1.47.0, which does
# not, and is exactly the build that made this pin meaningless before.
#
# By DIGEST, not by tag: a tag is a moving target, and a pin that moves is a
# pin that silently stops pinning. Update it deliberately, and regenerate both
# .sha256 files in the same commit.
: "${PACK_IMAGE:=debian:trixie-slim}"

# Does THIS mke2fs actually produce the same bytes twice? Two 1 MiB
# filesystems over an empty directory, a second apart so a clock that leaks in
# has moved. Costs about a tenth of a second and turns a silent property into
# a stated one.
reproducible_mke2fs() {
  local d a b
  # Checked, because an unchecked mktemp is an `rm -rf ""` and, worse, a
  # `mkdir -p "/root"` followed by two filesystem images written to the root of
  # the host's disk and never cleaned up. `rm -rf ""` is a no-op, so the
  # wreckage outlives the failure silently.
  d="$(mktemp -d)" || return 1
  [ -n "$d" ] || return 1
  mkdir -p "$d/root"
  mke2fs -q -F -t ext4 -b 4096 -U "$FS_UUID" -E hash_seed="$FS_HASH_SEED" \
    -d "$d/root" "$d/a.img" 1M 2>/dev/null || { rm -rf "$d"; return 1; }
  sleep 1
  mke2fs -q -F -t ext4 -b 4096 -U "$FS_UUID" -E hash_seed="$FS_HASH_SEED" \
    -d "$d/root" "$d/b.img" 1M 2>/dev/null || { rm -rf "$d"; return 1; }
  a="$(sha256sum < "$d/a.img")"; b="$(sha256sum < "$d/b.img")"
  rm -rf "$d"
  [ "$a" = "$b" ]
}

# Which toolchain packs: the pinned container by default, the host on request
# or when there is no Docker to pin with.
PACK_IN_CONTAINER=1
if [ "${PACK_LOCAL:-0}" = "1" ]; then
  PACK_IN_CONTAINER=0
  echo "==> PACK_LOCAL=1: packing with this host's e2fsprogs"
elif ! docker info >/dev/null 2>&1; then
  PACK_IN_CONTAINER=0
  echo "==> no usable Docker; packing with this host's e2fsprogs" >&2
fi

if [ "$PACK_IN_CONTAINER" = "1" ]; then
  echo "==> packing inside $PACK_IMAGE, so the pin means the same bytes everywhere"
elif reproducible_mke2fs; then
  echo "==> mke2fs is reproducible here; the pin this writes is meaningful"
else
  echo "==> WARNING: this mke2fs ($(mke2fs -V 2>&1 | head -1)) does not honour" >&2
  echo "    SOURCE_DATE_EPOCH, so the image it packs differs run to run and" >&2
  echo "    the hash written to ${OUT}.sha256 is good only for THIS build." >&2
  echo "    Every host you ship this image to still gets identical bytes --" >&2
  echo "    host-bootstrap.sh copies one file -- but a rebuild will not" >&2
  echo "    reproduce the pin. e2fsprogs 1.47.4 honours it; 1.47.0 does not." >&2
  echo "    Unset PACK_LOCAL and install Docker to pack in $PACK_IMAGE instead." >&2
fi

STAGED_BIN="scripts/rootfs/guest-agent"
TAR="$(mktemp -t pilots-rootfs-XXXXXX.tar)" || die "could not make a temporary tar"
ROOT="$(mktemp -d -t pilots-rootfs-XXXXXX)" || die "could not make a temporary root"
[ -n "$TAR" ] && [ -n "$ROOT" ] || die "mktemp produced an empty path"
CID=""

cleanup() {
  [ -n "$CID" ] && docker rm -f "$CID" >/dev/null 2>&1 || true
  # Each guarded: an empty variable makes `rm -rf` a no-op that LOOKS like a
  # cleanup, so a run whose mktemp failed left its wreckage behind reporting
  # success. Guarded here as well as at the assignment because cleanup runs on
  # every exit path, including ones taken before those assignments.
  [ -n "$TAR" ] && rm -rf "$TAR"
  [ -n "$ROOT" ] && rm -rf "$ROOT"
  [ -n "$STAGED_BIN" ] && rm -rf "$STAGED_BIN"
  return 0
}
trap cleanup EXIT

# -trimpath is load-bearing, not tidiness. Without it the binary embeds the
# absolute module-cache and source paths of whoever built it, so the same
# source produces different bytes for a user and for root -- and the pin test
# that checks the image carries the agent this tree builds fails for a reason
# that has nothing to do with the agent. With it the build is reproducible and
# the pin means what it says.
# REUSE_IMAGE packs the image that is already built instead of building it.
#
# For ONE caller: the reproducibility check, which packs the same image twice
# and compares. Two full builds cannot be compared, because the Dockerfile
# reaches the network -- `FROM ubuntu:24.04` is a tag rather than a digest,
# apt-get installs whatever the archive holds today, and the Node step pipes a
# setup script from deb.nodesource.com into bash. Any of those moving between
# two builds changes the image, which is a property of the base system rather
# than of anything this script does.
#
# What this script CAN promise is that packing is deterministic: the same
# filesystem in gives the same ext4 out, with no random uuid, hash seed or
# unclamped timestamp getting back in. That is the promise the check tests, and
# this is how it gets two packs of one image to compare.
if [ "${REUSE_IMAGE:-0}" = "1" ]; then
  docker image inspect "$IMAGE" >/dev/null 2>&1 \
    || die "REUSE_IMAGE=1 but there is no $IMAGE to reuse; build it first"
  echo "==> reusing the already-built $IMAGE"
else
  echo "==> building guest-agent (static)"
  ( cd apps/hostd && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
      go build -trimpath -ldflags="-s -w" -o "../../$STAGED_BIN" ./cmd/guest-agent )

  echo "==> docker build $IMAGE ($VARIANT, -f $DOCKERFILE)"
  docker build -q -t "$IMAGE" -f "$DOCKERFILE" scripts/rootfs
fi

echo "==> exporting container filesystem"
CID="$(docker create "$IMAGE")"
docker export "$CID" -o "$TAR"

echo "==> packing ext4 (${SIZE_MB}M)"
rm -f "$OUT"
# SOURCE_DATE_EPOCH and the two fixed ids are passed EXPLICITLY: the body
# below is a separate shell, so exporting them out here is not enough to be
# sure they arrive -- and if they silently did not, the build would still
# succeed and just stop being reproducible.
# The pack body, written to a file so the same text runs on the host and inside
# the container. One copy, because two copies of this would drift and the
# symptom would be two images that differ for a reason nobody could see.
PACK_SCRIPT="$(mktemp -t pilots-pack-XXXXXX.sh)"
cat > "$PACK_SCRIPT" <<'PACK'
set -eu
tar -xf "$TAR" -C "$ROOT"

  # Docker bind-mounts /etc/resolv.conf during build, so it cannot be written
  # in the Dockerfile -- it has to be written here, after export.
  #
  # ONE nameserver, and it is the gateway. hostd answers .internal there and
  # forwards everything else upstream, so listing a public resolver as well
  # would not be a fallback: a resolver that gets NXDOMAIN from the first
  # server does not try the second, and every .internal lookup that raced a
  # hostd restart would resolve to a public NXDOMAIN instead.
  rm -f "$ROOT/etc/resolv.conf"
  printf "nameserver %s\noptions timeout:1 attempts:2\n" "169.254.0.22" > "$ROOT/etc/resolv.conf"

  # The kernel boots /sbin/init; systemd lives elsewhere in the image.
  ln -sf /lib/systemd/systemd "$ROOT/sbin/init"
  rm -f "$ROOT/.dockerenv"

mke2fs -q -F -t ext4 -b 4096 -U "$FS_UUID" -E hash_seed="$FS_HASH_SEED" \
  -d "$ROOT" "$OUT" "${SIZE_MB}M"
PACK

if [ "$PACK_IN_CONTAINER" = "1" ]; then
  # The tar goes in, the image comes out, and nothing else is shared. The
  # extraction happens INSIDE, under the container's own fakeroot, so the
  # ownership and mode bits in the image are the tar's rather than whatever
  # this host's umask and uid would have imposed.
  PACK_DIR="$(mktemp -d -t pilots-pack-XXXXXX)" || die "could not make a pack directory"
  [ -n "$PACK_DIR" ] || die "mktemp produced an empty pack directory"
  cp "$TAR" "$PACK_DIR/rootfs.tar"
  cp "$PACK_SCRIPT" "$PACK_DIR/pack.sh"
  mkdir -p "$PACK_DIR/root"
  docker run --rm \
    -v "$PACK_DIR:/work" \
    -e TAR=/work/rootfs.tar -e ROOT=/work/root -e OUT=/work/out.img \
    -e SIZE_MB="$SIZE_MB" -e SOURCE_DATE_EPOCH="$SOURCE_DATE_EPOCH" \
    -e FS_UUID="$FS_UUID" -e FS_HASH_SEED="$FS_HASH_SEED" \
    -e HOST_UID="$(id -u)" -e HOST_GID="$(id -g)" \
    "$PACK_IMAGE" sh -euc '
      # Hand /work back to the invoking user on EVERY exit path.
      #
      # The container runs as root and extracts the tar into the bind mount, so
      # everything it writes there is root-owned on the host. The caller then
      # could not remove it: `rm -rf "$PACK_DIR"` failed with a screen of
      # Permission denied, and because the script runs under `set -e` that made
      # a build that had ALREADY produced a correct image exit 1 -- and leave
      # two gigabytes of extracted rootfs in /tmp every time.
      #
      # A trap rather than a line at the end, because a pack that fails halfway
      # leaves the same undeleteable tree, and that is exactly when somebody
      # wants to look at what it left.
      trap '"'"'chown -R "$HOST_UID:$HOST_GID" /work 2>/dev/null || true'"'"' EXIT
      # Installed here rather than baked into an image of our own, so the pin
      # is one upstream digest instead of a registry we would have to host.
      apt-get -qq update >/dev/null
      DEBIAN_FRONTEND=noninteractive apt-get -qq install -y --no-install-recommends         e2fsprogs fakeroot >/dev/null
      mke2fs -V 2>&1 | head -1
      fakeroot sh /work/pack.sh
    '
  mv "$PACK_DIR/out.img" "$OUT"
  rm -rf "$PACK_DIR"
else
  TAR="$TAR" ROOT="$ROOT" OUT="$OUT" SIZE_MB="$SIZE_MB" \
    SOURCE_DATE_EPOCH="$SOURCE_DATE_EPOCH" FS_UUID="$FS_UUID" \
    FS_HASH_SEED="$FS_HASH_SEED" fakeroot sh "$PACK_SCRIPT"
fi
rm -f "$PACK_SCRIPT"

sha256sum "$OUT" > "$OUT.sha256"

apparent="$(stat -c%s "$OUT")"
actual="$(( $(stat -c%b "$OUT") * $(stat -c%B "$OUT") ))"
echo "==> $OUT"
echo "    apparent: $(( apparent / 1024 / 1024 )) MiB"
echo "    actual:   $(( actual / 1024 / 1024 )) MiB (sparse)"
echo "    sha256:   $(cut -d' ' -f1 < "$OUT.sha256")"
