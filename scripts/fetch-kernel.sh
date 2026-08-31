#!/usr/bin/env bash
# Install the pinned guest kernel.
#
# Firecracker's boot-source takes an UNCOMPRESSED vmlinux ELF -- never a
# bzImage. If a future re-pin points at a compressed artifact, FC will reject
# it at boot with an unhelpful error.
set -euo pipefail

KERNEL_VERSION=vmlinux-6.1.158
SHA256=1982f8d5f1bc1680a36b0cdf126f605834b1633bba200d3281bccd53b86ff9ee
PREFIX="${PREFIX:-/opt/pilots}"
dest="$PREFIX/kernels/$KERNEL_VERSION/vmlinux.bin"

if [ -f "$dest" ] && echo "$SHA256  $dest" | sha256sum -c - >/dev/null 2>&1; then
  echo "kernel $KERNEL_VERSION already installed at $dest"
  exit 0
fi

base="https://storage.googleapis.com/e2b-prod-public-builds/kernels/${KERNEL_VERSION}"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT

fetched=""
for url in "$base/vmlinux.bin" "$base/amd64/vmlinux.bin"; do
  echo "==> trying $url"
  if curl -fsSL "$url" -o "$tmp/vmlinux.bin"; then fetched="$url"; break; fi
done
[ -n "$fetched" ] || { echo "could not download $KERNEL_VERSION" >&2; exit 1; }

echo "==> verifying sha256"
echo "${SHA256}  $tmp/vmlinux.bin" | sha256sum -c - || {
  echo "CHECKSUM MISMATCH -- refusing to install" >&2; exit 1; }

case "$(file -b "$tmp/vmlinux.bin" 2>/dev/null || echo unknown)" in
  *"ELF 64-bit"*) ;;
  *) echo "WARNING: artifact is not an ELF; firecracker needs an uncompressed vmlinux" >&2 ;;
esac

install -d -m0755 "$(dirname "$dest")"
install -m0644 "$tmp/vmlinux.bin" "$dest"
echo "==> installed $dest ($(stat -c%s "$dest") bytes) from $fetched"
