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

cd "$(dirname "$0")/.."

SIZE_MB="${SIZE_MB:-2048}"
OUT="${OUT:-scripts/rootfs/golden.ext4}"
IMAGE="${IMAGE:-pilots-golden-rootfs}"

# Three knobs aimed at making the image byte-reproducible, so that rebuilding
# it is idempotent and the pin in golden.ext4.sha256 can mean something.
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
# Making this hold everywhere means pinning the toolchain -- running the pack
# inside a container with a known e2fsprogs -- which also gets reproducibility
# ACROSS machines, something no combination of flags here can. That is not in
# this change; see #108.
: "${SOURCE_DATE_EPOCH:=1700000000}"
FS_UUID="${FS_UUID:-6f696c70-7473-4000-8000-676f6c64656e}"
FS_HASH_SEED="${FS_HASH_SEED:-70696c6f-7473-4000-8000-736565646564}"
export SOURCE_DATE_EPOCH

# Does THIS mke2fs actually produce the same bytes twice? Two 1 MiB
# filesystems over an empty directory, a second apart so a clock that leaks in
# has moved. Costs about a tenth of a second and turns a silent property into
# a stated one.
reproducible_mke2fs() {
  local d a b
  d="$(mktemp -d)"
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

if reproducible_mke2fs; then
  echo "==> mke2fs is reproducible here; the pin this writes is meaningful"
else
  echo "==> WARNING: this mke2fs ($(mke2fs -V 2>&1 | head -1)) does not honour" >&2
  echo "    SOURCE_DATE_EPOCH, so the image it packs differs run to run and" >&2
  echo "    the hash written to ${OUT}.sha256 is good only for THIS build." >&2
  echo "    Every host you ship this image to still gets identical bytes --" >&2
  echo "    host-bootstrap.sh copies one file -- but a rebuild will not" >&2
  echo "    reproduce the pin. e2fsprogs 1.47.4 honours it; 1.47.0 does not." >&2
fi

STAGED_BIN="scripts/rootfs/guest-agent"
TAR="$(mktemp -t pilots-rootfs-XXXXXX.tar)"
ROOT="$(mktemp -d -t pilots-rootfs-XXXXXX)"
CID=""

cleanup() {
  [ -n "$CID" ] && docker rm -f "$CID" >/dev/null 2>&1 || true
  rm -rf "$TAR" "$ROOT" "$STAGED_BIN"
}
trap cleanup EXIT

# -trimpath is load-bearing, not tidiness. Without it the binary embeds the
# absolute module-cache and source paths of whoever built it, so the same
# source produces different bytes for a user and for root -- and the pin test
# that checks the image carries the agent this tree builds fails for a reason
# that has nothing to do with the agent. With it the build is reproducible and
# the pin means what it says.
echo "==> building guest-agent (static)"
( cd apps/hostd && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w" -o "../../$STAGED_BIN" ./cmd/guest-agent )

echo "==> docker build $IMAGE"
docker build -q -t "$IMAGE" scripts/rootfs

echo "==> exporting container filesystem"
CID="$(docker create "$IMAGE")"
docker export "$CID" -o "$TAR"

echo "==> packing ext4 (${SIZE_MB}M)"
rm -f "$OUT"
# SOURCE_DATE_EPOCH and the two fixed ids are passed EXPLICITLY: the body
# below is a separate shell, so exporting them out here is not enough to be
# sure they arrive -- and if they silently did not, the build would still
# succeed and just stop being reproducible.
TAR="$TAR" ROOT="$ROOT" OUT="$OUT" SIZE_MB="$SIZE_MB" \
  SOURCE_DATE_EPOCH="$SOURCE_DATE_EPOCH" FS_UUID="$FS_UUID" \
  FS_HASH_SEED="$FS_HASH_SEED" fakeroot sh -euc '
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
'

sha256sum "$OUT" > "$OUT.sha256"

apparent="$(stat -c%s "$OUT")"
actual="$(( $(stat -c%b "$OUT") * $(stat -c%B "$OUT") ))"
echo "==> $OUT"
echo "    apparent: $(( apparent / 1024 / 1024 )) MiB"
echo "    actual:   $(( actual / 1024 / 1024 )) MiB (sparse)"
echo "    sha256:   $(cut -d' ' -f1 < "$OUT.sha256")"
