#!/usr/bin/env bash
# Install a pinned Firecracker + jailer, plus the CPU templates.
#
# The version is pinned and the checksum verified because snapshot
# compatibility is a function of the FC build: an unpinned upgrade can make
# existing snapshots unrestorable.
#
# The CPU templates matter for the same reason. FC memory snapshots carry raw
# CPUID, so a snapshot never restores across the Intel/AMD boundary. Pinning a
# template (T2/T2CL on Intel, T2A on AMD) normalises what the guest sees and
# lets later host generations of the SAME vendor join the fleet safely.
set -euo pipefail

FC_VERSION=1.16.1                      # released 2026-07-02
SHA256_X86_64=382a02a869e4d6d5cb14c40577f9545e8458021ea8b0b2d3fc10ec14d9c242e6
PREFIX="${PREFIX:-/opt/pilots}"

arch="$(uname -m)"
[ "$arch" = "x86_64" ] || { echo "unsupported arch: $arch" >&2; exit 1; }

if [ -x "$PREFIX/bin/firecracker" ] &&
   "$PREFIX/bin/firecracker" --version 2>/dev/null | grep -q "v$FC_VERSION"; then
  echo "firecracker v$FC_VERSION already installed at $PREFIX/bin"
  exit 0
fi

tarball="firecracker-v${FC_VERSION}-${arch}.tgz"
url="https://github.com/firecracker-microvm/firecracker/releases/download/v${FC_VERSION}/${tarball}"
tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT

echo "==> downloading $url"
curl -fsSL "$url" -o "$tmp/$tarball"

echo "==> verifying sha256"
echo "${SHA256_X86_64}  $tmp/$tarball" | sha256sum -c - || {
  echo "CHECKSUM MISMATCH -- refusing to install" >&2; exit 1; }

tar -xzf "$tmp/$tarball" -C "$tmp"
rel="$tmp/release-v${FC_VERSION}-${arch}"

install -d -m0755 "$PREFIX/bin" "$PREFIX/cpu-templates"
install -m0755 "$rel/firecracker-v${FC_VERSION}-${arch}" "$PREFIX/bin/firecracker"
install -m0755 "$rel/jailer-v${FC_VERSION}-${arch}"      "$PREFIX/bin/jailer"

for tpl in T2 T2CL T2A; do
  src="$rel/cpu-templates/${tpl}-v${FC_VERSION}.json"
  [ -f "$src" ] || src="$rel/${tpl}-v${FC_VERSION}.json"
  [ -f "$src" ] || { echo "missing cpu template: $tpl" >&2; exit 1; }
  install -m0644 "$src" "$PREFIX/cpu-templates/${tpl}.json"
done

echo "==> installed:"
"$PREFIX/bin/firecracker" --version | head -1
ls "$PREFIX/cpu-templates" 2>/dev/null | sed 's/^/    cpu-template: /' || true
