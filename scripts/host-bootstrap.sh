#!/usr/bin/env bash
# Turn a bare Ubuntu host into a pilots host.
#
#   scripts/host-bootstrap.sh <ip> [--peer <mesh-addr>]
#
# Idempotent by presence-check at every step, so re-running it upgrades a host
# and never breaks one. The first host is bootstrapped with no --peer and
# forms a cluster of one; every host after that is given any existing host's
# mesh address and joins through it.
#
# This is the whole of "add a host = give an IP". There is no registration, no
# allocator, and nothing to update anywhere else: the new host mints its own
# mesh identity, derives its own address from it, and announces itself by
# writing its own row.
set -euo pipefail

# Pinned versions. Everything the fleet runs is a fixed artifact -- a host that
# quietly installed a different Firecracker would produce snapshots its peers
# could not restore.
# Matched to what the engine was developed and tested against. Firecracker
# makes no promise of snapshot compatibility across versions, and a fleet
# running a different one than its template was built on fails at restore --
# reporting a bad snapshot rather than a version mismatch.
FC_VERSION="1.16.1"
CORROSION_VERSION="1.0.0"
KERNEL_VERSION="6.1.158"

REPO="$(cd "$(dirname "$0")/.." && pwd)"
IP="${1:-}"
PEER=""
BUCKET="${PILOT_S3_BUCKET:-}"
S3_ENDPOINT="${PILOT_S3_ENDPOINT:-}"
S3_KEY="${PILOT_S3_ACCESS_KEY:-}"
S3_SECRET="${PILOT_S3_SECRET_KEY:-}"
CORROSION_TOKEN="${PILOT_CORROSION_TOKEN:-}"
DOMAIN="${PILOT_WORKLOAD_DOMAIN:-pilotrun.app}"
SSH_OPTS="${SSH_OPTS:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null}"

shift || true
while [ $# -gt 0 ]; do
  case "$1" in
    --peer) PEER="${2:-}"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

[ -n "$IP" ] || { echo "usage: $0 <ip> [--peer <mesh-addr>]" >&2; exit 2; }
[ -n "$CORROSION_TOKEN" ] || {
  echo "PILOT_CORROSION_TOKEN must be set: it is the cluster's shared API secret" >&2
  exit 2
}

say() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
on_host() { ssh $SSH_OPTS "root@${IP}" "$@"; }

say "Bootstrapping ${IP}${PEER:+ (joining via ${PEER})}"

# ---------------------------------------------------------------------------
say "[1/9] Base packages, user and directories"
on_host bash -euo pipefail -s <<'REMOTE'
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq curl ca-certificates iproute2 iptables nftables \
  e2fsprogs wireguard-tools sqlite3 >/dev/null

# The uid Firecracker is jailed to. Never root, and in the kvm group so it can
# open /dev/kvm from inside the jail.
id -u pilot-vm >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin pilot-vm
usermod -aG kvm pilot-vm

mkdir -p /var/lib/pilots/{machines,templates,corrosion} \
         /var/cache/pilots /run/pilots/corrosion /opt/pilots/{bin,kernels} /etc/pilots
REMOTE

# ---------------------------------------------------------------------------
say "[2/9] Kernel settings the engine depends on"
on_host bash -euo pipefail -s <<'REMOTE'
# The fault handler needs userfaultfd from an unprivileged process. Without
# this every restore fails at the handshake, reported as a permission error
# that names nothing useful.
cat >/etc/sysctl.d/60-pilots.conf <<'SYSCTL'
vm.unprivileged_userfaultfd = 1
SYSCTL
sysctl -q --system

# The block server needs network block devices. nbds_max is fixed at load
# time, so it goes in a modprobe conf rather than being set later.
cat >/etc/modules-load.d/pilots.conf <<'MODULES'
nbd
wireguard
MODULES
cat >/etc/modprobe.d/pilots-nbd.conf <<'MODPROBE'
options nbd nbds_max=64
MODPROBE
modprobe -r nbd 2>/dev/null || true
modprobe nbd nbds_max=64
modprobe wireguard
REMOTE

# ---------------------------------------------------------------------------
say "[3/9] Firecracker and jailer v${FC_VERSION}"
on_host bash -euo pipefail -s <<REMOTE
FC_DIR="/opt/firecracker/v${FC_VERSION}"
if [ -x "\$FC_DIR/firecracker" ] && "\$FC_DIR/firecracker" --version 2>&1 | grep -q "v${FC_VERSION}"; then
  echo "  already installed"
else
  mkdir -p "\$FC_DIR"
  ARCH=\$(uname -m)
  TMP=\$(mktemp -d); trap "rm -rf '\$TMP'" EXIT
  curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/v${FC_VERSION}/firecracker-v${FC_VERSION}-\${ARCH}.tgz" -o "\$TMP/fc.tgz"
  tar -xzf "\$TMP/fc.tgz" -C "\$TMP"
  install -m 0755 "\$(find "\$TMP" -name "firecracker-v${FC_VERSION}-\${ARCH}" -type f | head -1)" "\$FC_DIR/firecracker"
  install -m 0755 "\$(find "\$TMP" -name "jailer-v${FC_VERSION}-\${ARCH}" -type f | head -1)" "\$FC_DIR/jailer"
  echo "  installed v${FC_VERSION}"
fi
ln -sfn "\$FC_DIR/firecracker" /opt/pilots/bin/firecracker
ln -sfn "\$FC_DIR/jailer" /opt/pilots/bin/jailer
REMOTE

# ---------------------------------------------------------------------------
say "[4/9] Corrosion v${CORROSION_VERSION}"
on_host bash -euo pipefail -s <<REMOTE
# Pinned from day one, deliberately. The v0.x on-disk store cannot be upgraded
# in place, and starting on 1.0.0 is how we never write the migration.
if [ -x /opt/pilots/bin/corrosion ] && /opt/pilots/bin/corrosion --version 2>&1 | grep -q "${CORROSION_VERSION}"; then
  echo "  already installed"
else
  TMP=\$(mktemp -d); trap "rm -rf '\$TMP'" EXIT
  curl -fsSL "https://github.com/superfly/corrosion/releases/download/v${CORROSION_VERSION}/corrosion-x86_64-unknown-linux-gnu.tar.gz" -o "\$TMP/c.tgz"
  tar -xzf "\$TMP/c.tgz" -C "\$TMP"
  install -m 0755 "\$(find "\$TMP" -name corrosion -type f | head -1)" /opt/pilots/bin/corrosion
  echo "  installed v${CORROSION_VERSION}"
fi
ln -sfn /opt/pilots/bin/corrosion /usr/local/bin/corrosion
REMOTE

# ---------------------------------------------------------------------------
say "[5/9] Guest kernel and golden rootfs"
scp $SSH_OPTS -q "${REPO}/scripts/rootfs/golden.ext4" "root@${IP}:/var/lib/pilots/templates/golden.ext4" 2>/dev/null \
  || echo "  no local golden rootfs; the host will need one before creating machines"
if [ -f "/opt/pilots/kernels/vmlinux-${KERNEL_VERSION}/vmlinux.bin" ]; then
  on_host "mkdir -p /opt/pilots/kernels/vmlinux-${KERNEL_VERSION}"
  scp $SSH_OPTS -q "/opt/pilots/kernels/vmlinux-${KERNEL_VERSION}/vmlinux.bin" \
    "root@${IP}:/opt/pilots/kernels/vmlinux-${KERNEL_VERSION}/vmlinux.bin"
fi

# ---------------------------------------------------------------------------
say "[6/9] hostd"
( cd "${REPO}/apps/hostd" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/pilots-hostd ./cmd/hostd )
scp $SSH_OPTS -q /tmp/pilots-hostd "root@${IP}:/opt/pilots/bin/hostd.new"
on_host "chmod 0755 /opt/pilots/bin/hostd.new && mv /opt/pilots/bin/hostd.new /opt/pilots/bin/hostd"
rm -f /tmp/pilots-hostd

# ---------------------------------------------------------------------------
say "[7/9] Mesh identity and host configuration"
# The identity is minted ON the host and never leaves it. Its mesh address is
# derived from the public half, so nothing has to hand one out -- which is what
# keeps adding a host from needing a registry.
on_host bash -euo pipefail -s <<'REMOTE'
if [ ! -s /var/lib/pilots/mesh.key ]; then
  umask 077
  wg genkey > /var/lib/pilots/mesh.key
fi
chmod 0600 /var/lib/pilots/mesh.key
REMOTE

PUBKEY=$(on_host "wg pubkey < /var/lib/pilots/mesh.key")
# Asked for, never recomputed. Deriving the address here as well would be a
# second implementation of the same rule, and the two disagreeing is silent:
# corrosion binds one address while the mesh answers on another, and gossip
# never arrives.
MESH_ADDR=$(on_host "/opt/pilots/bin/hostd mesh-addr")
HOST_ID="host-$(echo "$PUBKEY" | tr -d '=/+' | tr 'A-Z' 'a-z' | cut -c1-12)"
echo "  host id   ${HOST_ID}"
echo "  mesh addr ${MESH_ADDR}"

on_host bash -euo pipefail -s <<REMOTE
cat >/etc/pilots/config <<CONF
PILOT_HOST_ID=${HOST_ID}
PILOT_PUBLIC_IP=${IP}
PILOT_WORKLOAD_DOMAIN=${DOMAIN}
PILOT_STATE_BACKEND=corrosion
PILOT_MESH_ENABLED=1
PILOT_CORROSION_ADDR=127.0.0.1:51002
PILOT_CORROSION_TOKEN=${CORROSION_TOKEN}
PILOT_S3_ENDPOINT=${S3_ENDPOINT}
PILOT_S3_BUCKET=${BUCKET}
PILOT_S3_ACCESS_KEY=${S3_KEY}
PILOT_S3_SECRET_KEY=${S3_SECRET}
PILOT_TEMPLATE_ROOTFS=/var/lib/pilots/templates/golden.ext4
PILOT_KERNEL=/opt/pilots/kernels/vmlinux-${KERNEL_VERSION}/vmlinux.bin
PILOT_FIRECRACKER=/opt/pilots/bin/firecracker
PILOT_JAILER=/opt/pilots/bin/jailer
CONF
chmod 0600 /etc/pilots/config
REMOTE

# ---------------------------------------------------------------------------
say "[8/9] Corrosion schema, config and units"
scp $SSH_OPTS -q "${REPO}/apps/hostd/internal/state/schema.sql" \
  "root@${IP}:/var/lib/pilots/corrosion/schema.sql"

# The schema file must be BYTE-IDENTICAL on every host: corrosion does not
# replicate DDL, so a host with a different one silently diverges.
BOOTSTRAP_LINE=""
[ -n "$PEER" ] && BOOTSTRAP_LINE="\"[${PEER}]:51001\""

on_host bash -euo pipefail -s <<REMOTE
cat >/var/lib/pilots/corrosion/config.toml <<CONF
[db]
path = "/var/lib/pilots/corrosion/store.db"
schema_paths = ["/var/lib/pilots/corrosion/schema.sql"]

[gossip]
addr = "[${MESH_ADDR}]:51001"
bootstrap = [${BOOTSTRAP_LINE}]
# Pinned from the smallest MTU any host could have. Left to discover it,
# QUIC overestimates across a heterogeneous underlay and gossip black-holes
# in a way that presents as the cluster flapping at random.
max_mtu = 1232
# The mesh already authenticates and encrypts every byte.
plaintext = true

[api]
addr = "127.0.0.1:51002"

[api.authz]
bearer-token = "${CORROSION_TOKEN}"

[admin]
path = "/run/pilots/corrosion/admin.sock"
CONF

# hostd brings the mesh up, and gossip rides the mesh -- so corrosion cannot
# reach its bootstrap peer until hostd has run. Ordering them the other way
# leaves a corrosion that sits retrying an address with no route to it.
cat >/etc/systemd/system/pilots-mesh.service <<'UNIT'
[Unit]
Description=pilots mesh interface
Before=corrosion.service
[Service]
Type=oneshot
RemainAfterExit=yes
EnvironmentFile=/etc/pilots/config
ExecStart=/opt/pilots/bin/hostd mesh-up
[Install]
WantedBy=multi-user.target
UNIT

cat >/etc/systemd/system/corrosion.service <<'UNIT'
[Unit]
Description=corrosion (pilots cluster state)
After=network-online.target pilots-mesh.service
Requires=pilots-mesh.service
[Service]
ExecStart=/opt/pilots/bin/corrosion agent --config /var/lib/pilots/corrosion/config.toml
Restart=always
RestartSec=2
[Install]
WantedBy=multi-user.target
UNIT

cat >/etc/systemd/system/hostd.service <<'UNIT'
[Unit]
Description=pilots hostd
After=network-online.target corrosion.service
Wants=corrosion.service
[Service]
EnvironmentFile=/etc/pilots/config
ExecStart=/opt/pilots/bin/hostd
Restart=always
RestartSec=2
# The machines outlive the daemon: a restart re-adopts them rather than
# taking every workload on the host down with it.
KillMode=process
[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable --now pilots-mesh corrosion hostd
REMOTE

# ---------------------------------------------------------------------------
say "[9/9] Verifying"
on_host bash -euo pipefail -s <<'REMOTE'
for i in $(seq 60); do
  curl -sf http://127.0.0.1:8080/v1/health >/dev/null 2>&1 && break
  sleep 1
done
curl -sf http://127.0.0.1:8080/v1/health >/dev/null || { echo "  hostd is not serving" >&2; exit 1; }
echo "  hostd serving"
wg show pilots0 2>/dev/null | grep -c peer | xargs -I{} echo "  mesh peers: {}"
systemctl is-active --quiet corrosion && echo "  corrosion running"
REMOTE

echo
echo "Host ${IP} is up."
echo "  host id:   ${HOST_ID}"
echo "  mesh addr: ${MESH_ADDR}"
echo
echo "Bootstrap the next host with:"
echo "  scripts/host-bootstrap.sh <ip> --peer ${MESH_ADDR}"
