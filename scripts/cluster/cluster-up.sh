#!/usr/bin/env bash
# Bring up the local cluster the phase gate runs on.
#
#   scripts/cluster/cluster-up.sh
#
# Its job ends at "N Ubuntu VMs with /dev/kvm and an IP". Turning those into
# pilots hosts is host-bootstrap.sh's job, and the gate requires that the
# cluster comes up through the bootstrap script alone -- so nothing here
# installs or configures anything pilots-specific.
set -euo pipefail

cd "$(dirname "$0")"
# shellcheck source=config.sh
source ./config.sh

# Previous runs' node addresses, for NEW_ONLY: a node left powered off has no
# DHCP lease to read, but its address is still the one it will come back on.
#
# Read the one key rather than sourcing the file: the file also carries NODES
# from the last run, and sourcing it would override the NODES the caller just
# asked for -- which is exactly how a node gets added.
PREV_NODE_IPS=""
if [ -f "$STATE_FILE" ]; then
  PREV_NODE_IPS=$(sed -n 's/^NODE_IPS="\(.*\)"$/\1/p' "$STATE_FILE" | tail -1)
fi

say() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

command -v virsh >/dev/null || { echo "virsh not installed" >&2; exit 1; }
command -v virt-install >/dev/null || { echo "virt-install not installed" >&2; exit 1; }
command -v cloud-localds >/dev/null || { echo "cloud-localds not installed (cloud-image-utils)" >&2; exit 1; }

[ -r /dev/kvm ] || { echo "/dev/kvm is not readable" >&2; exit 1; }
if [ "$(cat /sys/module/kvm_amd/parameters/nested 2>/dev/null || cat /sys/module/kvm_intel/parameters/nested 2>/dev/null)" != "1" ] &&
   [ "$(cat /sys/module/kvm_amd/parameters/nested 2>/dev/null || cat /sys/module/kvm_intel/parameters/nested 2>/dev/null)" != "Y" ]; then
  echo "nested virtualisation is off; Firecracker cannot run inside the nodes" >&2
  exit 1
fi
[ -f "${SSH_KEY}.pub" ] || { echo "no ssh key at ${SSH_KEY}.pub" >&2; exit 1; }

SUDO=""
[ "$(id -u)" = 0 ] || SUDO="sudo"

say "Network ${NET_NAME}"
if ! $SUDO virsh net-info "$NET_NAME" >/dev/null 2>&1; then
  TMP=$(mktemp)
  cat >"$TMP" <<XML
<network>
  <name>${NET_NAME}</name>
  <forward mode='nat'/>
  <bridge name='${NET_BRIDGE}' stp='on' delay='0'/>
  <ip address='${NET_SUBNET}.1' netmask='255.255.255.0'>
    <dhcp><range start='${NET_SUBNET}.100' end='${NET_SUBNET}.200'/></dhcp>
  </ip>
</network>
XML
  $SUDO virsh net-define "$TMP" >/dev/null
  rm -f "$TMP"
fi
$SUDO virsh net-start "$NET_NAME" >/dev/null 2>&1 || true
$SUDO virsh net-autostart "$NET_NAME" >/dev/null 2>&1 || true

# Without these the nodes never get a DHCP lease and the whole thing looks
# like libvirt being broken.
if command -v ufw >/dev/null && $SUDO ufw status 2>/dev/null | grep -q "Status: active"; then
  $SUDO ufw allow in on "$NET_BRIDGE" >/dev/null 2>&1 || true
  $SUDO ufw allow out on "$NET_BRIDGE" >/dev/null 2>&1 || true
fi

say "Base image"
$SUDO mkdir -p "$WORK_DIR"
BASE="${WORK_DIR}/base.qcow2"
if [ ! -f "$BASE" ]; then
  echo "  downloading $BASE_IMAGE_URL"
  $SUDO curl -fsSL "$BASE_IMAGE_URL" -o "$BASE"
else
  echo "  already present"
fi

PUBKEY=$(cat "${SSH_KEY}.pub")

for i in $(seq 1 "$NODES"); do
  NODE="${NODE_PREFIX}-${i}"
  say "Node ${NODE}"

  if $SUDO virsh dominfo "$NODE" >/dev/null 2>&1; then
    # NEW_ONLY leaves nodes that already exist exactly as they are, running
    # or not. Adding a node to a fleet must not restart the fleet -- and the
    # gate adds one while a host is deliberately powered off, so starting it
    # back up here would quietly undo the failure being tested.
    if [ "${NEW_ONLY:-0}" = 1 ]; then
      echo "  already defined (left alone)"
    else
      $SUDO virsh start "$NODE" >/dev/null 2>&1 || true
      echo "  already defined"
    fi
    continue
  fi

  DISK="${WORK_DIR}/${NODE}.qcow2"
  SEED="${WORK_DIR}/${NODE}-seed.iso"

  # A thin overlay on the base image: each node costs its writes, not a copy.
  $SUDO qemu-img create -f qcow2 -F qcow2 -b "$BASE" "$DISK" "${NODE_DISK_GB}G" >/dev/null
  $SUDO qemu-img resize "$DISK" "${NODE_DISK_GB}G" >/dev/null 2>&1 || true

  TMPD=$(mktemp -d)
  cat >"${TMPD}/user-data" <<CLOUD
#cloud-config
hostname: ${NODE}
ssh_pwauth: false
disable_root: false
users:
  - name: root
    ssh_authorized_keys:
      - ${PUBKEY}
CLOUD
  printf 'instance-id: %s\nlocal-hostname: %s\n' "$NODE" "$NODE" >"${TMPD}/meta-data"
  $SUDO cloud-localds "$SEED" "${TMPD}/user-data" "${TMPD}/meta-data"
  rm -rf "$TMPD"

  # host-passthrough is what exposes the CPU's virtualisation extensions to
  # the guest. Without it Firecracker cannot run inside these nodes at all,
  # and the failure is a missing /dev/kvm rather than anything that names CPU
  # features.
  $SUDO virt-install \
    --name "$NODE" \
    --memory "$((NODE_RAM_GB * 1024))" \
    --vcpus "$NODE_VCPUS" \
    --cpu host-passthrough \
    --disk "path=${DISK},format=qcow2,bus=virtio" \
    --disk "path=${SEED},device=cdrom" \
    --os-variant "$OS_VARIANT" \
    --network "network=${NET_NAME},model=virtio" \
    --import --graphics none --noautoconsole >/dev/null
  echo "  created"
done

say "Waiting for addresses"
IPS=()
for i in $(seq 1 "$NODES"); do
  NODE="${NODE_PREFIX}-${i}"
  IP=""
  for _ in $(seq 120); do
    IP=$($SUDO virsh domifaddr "$NODE" --source lease 2>/dev/null | awk '/ipv4/ {print $4}' | cut -d/ -f1 | head -1)
    [ -n "$IP" ] && break
    # A node this run deliberately left powered off has no lease to wait for.
    if [ "${NEW_ONLY:-0}" = 1 ] && ! $SUDO virsh domstate "$NODE" 2>/dev/null | grep -q running; then
      break
    fi
    sleep 2
  done
  if [ -z "$IP" ]; then
    if [ "${NEW_ONLY:-0}" = 1 ]; then
      echo "  ${NODE} is not running; keeping its last known address"
      IP=$(echo "$PREV_NODE_IPS" | awk -v n="$i" '{print $n}')
      [ -n "$IP" ] || { echo "  ${NODE} has no known address" >&2; exit 1; }
    else
      echo "  ${NODE} never got a lease" >&2; exit 1
    fi
  fi
  echo "  ${NODE} ${IP}"
  IPS+=("$IP")
done

say "Waiting for ssh"
for ip in "${IPS[@]}"; do
  for _ in $(seq 60); do
    ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=3 -i "$SSH_KEY" "root@${ip}" true 2>/dev/null && break
    sleep 2
  done
done

# Keep keys this script does not own. cluster-bootstrap.sh puts the cluster's
# shared secrets here, and truncating the file on a later cluster-up would
# take the fleet's corrosion token and agent secret with it -- leaving a live
# cluster that no script can talk to any more.
PRESERVED=""
if [ -f "$STATE_FILE" ]; then
  PRESERVED=$(grep -vE '^(#|NODES=|NODE_IPS=)' "$STATE_FILE" || true)
fi
{
  echo "# Written by cluster-up.sh"
  echo "NODES=${NODES}"
  echo "NODE_IPS=\"${IPS[*]}\""
  [ -n "$PRESERVED" ] && echo "$PRESERVED"
} > "$STATE_FILE"

say "Cluster up"
printf '  %s\n' "${IPS[@]}"
echo
echo "Bootstrap them into a fleet with:"
echo "  scripts/cluster/cluster-bootstrap.sh"
