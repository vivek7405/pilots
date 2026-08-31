#!/usr/bin/env bash
# Boot the golden rootfs under an unmodified Firecracker and wait for a login
# prompt. This is the Phase 1 gate: it proves the kernel and rootfs are good
# before Phase 2 writes a line of the FC driver.
#
# Deliberately minimal -- no tap device and no network-interfaces call -- so it
# needs no root. /dev/kvm is world-writable on the dev laptop and on CI once
# the udev rule is applied; creating taps would need privileges, and that is
# Phase 2's problem, not the boot gate's.
set -euo pipefail

cd "$(dirname "$0")/.."

PREFIX="${PREFIX:-/opt/pilots}"
FC="${FC:-$PREFIX/bin/firecracker}"
KERNEL="${KERNEL:-$PREFIX/kernels/vmlinux-6.1.158/vmlinux.bin}"
ROOTFS="${ROOTFS:-scripts/rootfs/golden.ext4}"
VCPUS="${VCPUS:-1}"
MEM_MIB="${MEM_MIB:-256}"
DEADLINE="${DEADLINE:-90}"
KEEP=0

while [ $# -gt 0 ]; do
  case "$1" in
    --rootfs) ROOTFS="$2"; shift 2 ;;
    --kernel) KERNEL="$2"; shift 2 ;;
    --keep)   KEEP=1; shift ;;
    -h|--help) sed -n '2,12p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

for f in "$FC" "$KERNEL" "$ROOTFS"; do
  [ -e "$f" ] || { echo "missing: $f" >&2; exit 1; }
done
[ -w /dev/kvm ] || { echo "/dev/kvm is not writable" >&2; exit 1; }

API="$(mktemp -u -t pilots-dev-vm-XXXXXX.sock)"
LOG="$(mktemp -t pilots-dev-vm-XXXXXX.log)"
# Copy the image so a repeat run always starts from a clean rootfs.
SCRATCH="$(mktemp -t pilots-rootfs-XXXXXX.ext4)"
FC_PID=""

cleanup() {
  [ -n "$FC_PID" ] && [ "$KEEP" = 0 ] && kill "$FC_PID" 2>/dev/null || true
  [ "$KEEP" = 0 ] && rm -f "$API" "$SCRATCH" || true
}
trap cleanup EXIT

cp --reflink=auto "$ROOTFS" "$SCRATCH"

api() {
  curl -sS --unix-socket "$API" -X PUT "http://localhost$1" \
    -H 'Content-Type: application/json' -d "$2"
}

echo "==> starting firecracker"
"$FC" --api-sock "$API" >"$LOG" 2>&1 &
FC_PID=$!

for _ in $(seq 1 100); do [ -S "$API" ] && break; sleep 0.05; done
[ -S "$API" ] || { echo "API socket never appeared" >&2; cat "$LOG"; exit 1; }

api /machine-config "{\"vcpu_count\":$VCPUS,\"mem_size_mib\":$MEM_MIB,\"smt\":false}"

# Boot args are the platform's constants. The ip= addresses match
# scripts/rootfs/eth0.network exactly; they are identical on every machine and
# every host, which is what makes snapshots host-agnostic.
api /boot-source "$(cat <<JSON
{"kernel_image_path":"$KERNEL",
 "boot_args":"console=ttyS0 reboot=k panic=1 pci=off ro root=/dev/vda clocksource=kvm-clock random.trust_cpu=on i8042.nokbd i8042.noaux ipv6.disable=0 ipv6.autoconf=1 ip=169.254.0.21::169.254.0.22:255.255.255.252:instance:eth0:off:"}
JSON
)"

api /drives/rootfs "{\"drive_id\":\"rootfs\",\"path_on_host\":\"$SCRATCH\",\"is_root_device\":true,\"is_read_only\":true}"
api /actions '{"action_type":"InstanceStart"}'

echo "==> waiting up to ${DEADLINE}s for a login prompt"
start=$SECONDS
while [ $(( SECONDS - start )) -lt "$DEADLINE" ]; do
  if grep -q "login:" "$LOG" 2>/dev/null; then
    took=$(( SECONDS - start ))

    # A login prompt only proves the kernel and init are good. The rootfs is
    # not actually usable unless the agent systemd baked in came up too --
    # every later phase reaches the guest through it.
    if ! grep -q "Started.*guest-agent" "$LOG" 2>/dev/null; then
      echo "==> BOOT REACHED LOGIN in ${took}s, but guest-agent.service did not start" >&2
      grep -iE "guest.agent" "$LOG" >&2 || echo "    (no guest-agent lines in the serial log)" >&2
      exit 1
    fi

    echo "==> BOOT OK in ${took}s (guest-agent.service started)"
    [ "$KEEP" = 1 ] && echo "    VM left running (pid $FC_PID, api $API)"
    exit 0
  fi
  kill -0 "$FC_PID" 2>/dev/null || { echo "firecracker exited early" >&2; tail -40 "$LOG"; exit 1; }
  sleep 0.5
done

echo "==> TIMEOUT after ${DEADLINE}s; last 40 lines:" >&2
tail -40 "$LOG" >&2
exit 1
