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

STAGED_BIN="scripts/rootfs/guest-agent"
TAR="$(mktemp -t pilots-rootfs-XXXXXX.tar)"
ROOT="$(mktemp -d -t pilots-rootfs-XXXXXX)"
CID=""

cleanup() {
  [ -n "$CID" ] && docker rm -f "$CID" >/dev/null 2>&1 || true
  rm -rf "$TAR" "$ROOT" "$STAGED_BIN"
}
trap cleanup EXIT

echo "==> building guest-agent (static)"
( cd apps/hostd && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w" -o "../../$STAGED_BIN" ./cmd/guest-agent )

echo "==> docker build $IMAGE"
docker build -q -t "$IMAGE" scripts/rootfs

echo "==> exporting container filesystem"
CID="$(docker create "$IMAGE")"
docker export "$CID" -o "$TAR"

echo "==> packing ext4 (${SIZE_MB}M)"
rm -f "$OUT"
TAR="$TAR" ROOT="$ROOT" OUT="$OUT" SIZE_MB="$SIZE_MB" fakeroot sh -euc '
  tar -xf "$TAR" -C "$ROOT"

  # Docker bind-mounts /etc/resolv.conf during build, so it cannot be written
  # in the Dockerfile -- it has to be written here, after export.
  rm -f "$ROOT/etc/resolv.conf"
  printf "nameserver 8.8.8.8\nnameserver 1.1.1.1\n" > "$ROOT/etc/resolv.conf"

  # The kernel boots /sbin/init; systemd lives elsewhere in the image.
  ln -sf /lib/systemd/systemd "$ROOT/sbin/init"
  rm -f "$ROOT/.dockerenv"

  mke2fs -q -F -t ext4 -b 4096 -d "$ROOT" "$OUT" "${SIZE_MB}M"
'

sha256sum "$OUT" > "$OUT.sha256"

apparent="$(stat -c%s "$OUT")"
actual="$(( $(stat -c%b "$OUT") * $(stat -c%B "$OUT") ))"
echo "==> $OUT"
echo "    apparent: $(( apparent / 1024 / 1024 )) MiB"
echo "    actual:   $(( actual / 1024 / 1024 )) MiB (sparse)"
echo "    sha256:   $(cut -d' ' -f1 < "$OUT.sha256")"
