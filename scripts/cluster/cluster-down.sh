#!/usr/bin/env bash
# Tear the local cluster down.
set -euo pipefail
cd "$(dirname "$0")"
# shellcheck source=config.sh
source ./config.sh

SUDO=""
[ "$(id -u)" = 0 ] || SUDO="sudo"

# Every domain carrying the prefix, not 1..$NODES.
#
# NODES here is whatever config.sh defaults to unless the caller passes one,
# and it has no idea how big the rig actually is: a fleet brought up with
# NODES=5 and torn down with a bare cluster-down.sh used to leave hosts 4 and
# 5 defined, holding their disks, with the state file that named them already
# deleted. Asking libvirt what exists cannot get that wrong, and it collects
# a stray host however it came to be there.
#
# Anchored on the prefix so this cannot reach the predecessor rig's
# pilot-node-* domains, which still exist on some machines and share nothing
# with these but a working directory.
NODES_FOUND=0
while read -r NODE; do
  [ -n "$NODE" ] || continue
  case "$NODE" in
    "${NODE_PREFIX}-"*) ;;
    *) continue ;;
  esac
  $SUDO virsh destroy "$NODE" >/dev/null 2>&1 || true
  $SUDO virsh undefine "$NODE" --remove-all-storage >/dev/null 2>&1 || true
  echo "removed ${NODE}"
  NODES_FOUND=$((NODES_FOUND + 1))
done < <($SUDO virsh list --all --name 2>/dev/null || true)

[ "$NODES_FOUND" = 0 ] && echo "no ${NODE_PREFIX}-* domains to remove"

# The base image stays: re-downloading it on every cycle is minutes for
# nothing.
rm -f "$STATE_FILE"
echo "cluster down (base image kept at ${WORK_DIR}/base.qcow2)"
