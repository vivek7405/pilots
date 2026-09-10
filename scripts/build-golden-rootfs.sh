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

# The three knobs that make the image byte-reproducible, and therefore make
# the pin in golden.ext4.sha256 mean something.
#
# Without them the same tree produces a different image every run, and
# host-bootstrap.sh's pin check -- which refuses to ship anything that does
# not match -- becomes a check that can never pass. It stopped a rig being
# bootstrapped from a clean checkout: committed pin, image on disk and fresh
# rebuild were three different hashes.
#
# All three are load-bearing; each was verified by building twice and diffing:
#   -U            without it mke2fs picks a random filesystem UUID per run
#   hash_seed     without it the directory hash seed is random, even with -U
#   SOURCE_DATE_EPOCH  without it the superblock carries this run's clock
#
# The epoch also clamps the inode timestamps, so the file mtimes that come out
# of `docker export` do not have to be normalised by hand -- an image whose
# files were dated 2030 built to the same bytes. The value is a constant and
# not `date +%s`: reproducible has to mean across time, not within one run.
# Changing the image's CONTENT changes the hash regardless, which is the point.
#
# The scope of the guarantee, because it is narrower than it looks: the same
# tree on the same TOOLCHAIN builds the same bytes. A different e2fsprogs
# lays the filesystem out differently and a different Go builds a different
# agent, so a CI runner and a developer laptop do not agree -- measured, not
# assumed. That is enough to make a rebuild idempotent, which is what was
# missing. It is NOT enough to make the committed pin mean the same thing on
# another machine; that needs the toolchain pinned too, or the image
# published and downloaded rather than rebuilt. See #108.
: "${SOURCE_DATE_EPOCH:=1700000000}"
FS_UUID="${FS_UUID:-6f696c70-7473-4000-8000-676f6c64656e}"
FS_HASH_SEED="${FS_HASH_SEED:-70696c6f-7473-4000-8000-736565646564}"
export SOURCE_DATE_EPOCH

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
