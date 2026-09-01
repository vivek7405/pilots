#!/usr/bin/env bash
# Tear the local cluster down.
set -euo pipefail
cd "$(dirname "$0")"
# shellcheck source=config.sh
source ./config.sh

SUDO=""
[ "$(id -u)" = 0 ] || SUDO="sudo"

for i in $(seq 1 "${NODES}"); do
  NODE="${NODE_PREFIX}-${i}"
  $SUDO virsh destroy "$NODE" >/dev/null 2>&1 || true
  $SUDO virsh undefine "$NODE" --remove-all-storage >/dev/null 2>&1 || true
  echo "removed ${NODE}"
done

# The base image stays: re-downloading it on every cycle is minutes for
# nothing.
rm -f "$STATE_FILE"
echo "cluster down (base image kept at ${WORK_DIR}/base.qcow2)"
