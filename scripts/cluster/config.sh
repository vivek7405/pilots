#!/usr/bin/env bash
# Shared configuration for the local three-node cluster.
#
# Every value is env-overridable:
#   NODES=1 scripts/cluster/cluster-up.sh    (prove the pipeline with one)
#   NODES=3 scripts/cluster/cluster-up.sh    (what the phase gate runs on)

# Three nodes is what the gate needs: one to kill, and two survivors that
# must agree on which of them rescues what.
: "${NODES:=3}"

# Per node. Firecracker runs NESTED inside these, so they need real memory --
# a guest's memory is resident in its host, and the host is one of these VMs.
: "${NODE_RAM_GB:=10}"
: "${NODE_VCPUS:=4}"
: "${NODE_DISK_GB:=40}"

# libvirt NAT network. The nodes route to each other on it, which is the
# underlay the WireGuard mesh runs over.
: "${NET_NAME:=pilots-net}"
: "${NET_BRIDGE:=virpilots0}"
: "${NET_SUBNET:=192.168.124}"

: "${NODE_PREFIX:=pilot-node}"

# Ubuntu 24.04, which ships cloud-init.
: "${BASE_IMAGE_URL:=https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img}"
: "${OS_VARIANT:=ubuntu24.04}"

: "${WORK_DIR:=/var/lib/libvirt/images/pilots-cluster}"
: "${SSH_KEY:=$HOME/.ssh/id_ed25519}"
: "${STATE_FILE:=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/cluster.env}"
