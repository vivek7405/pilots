#!/usr/bin/env bash
# Turn the local cluster's VMs into a pilots fleet.
#
#   scripts/cluster/cluster-bootstrap.sh
#
# Nothing here knows anything host-bootstrap.sh does not: the first node
# bootstraps a cluster of one, and every node after joins through the first
# one's mesh address. That is the whole of "add a host = give an IP", and the
# gate requires it to be true.
set -euo pipefail

cd "$(dirname "$0")"
# shellcheck source=config.sh
source ./config.sh
REPO="$(cd ../.. && pwd)"

[ -f "$STATE_FILE" ] || { echo "no cluster; run cluster-up.sh first" >&2; exit 1; }
# shellcheck source=/dev/null
source "$STATE_FILE"
read -ra IPS <<< "$NODE_IPS"

# One shared secret for the cluster's state API, and one bucket. Both are
# fleet-wide by nature: a host with a different token cannot read the cluster,
# and a host with a different bucket cannot restore anyone else's machines.
export PILOT_CORROSION_TOKEN="${PILOT_CORROSION_TOKEN:-$(head -c 32 /dev/urandom | base64 | tr -d '=/+')}"
export PILOT_S3_ENDPOINT="${PILOT_S3_ENDPOINT:-http://${NET_SUBNET}.1:9000}"
export PILOT_S3_BUCKET="${PILOT_S3_BUCKET:-pilots}"
export PILOT_S3_ACCESS_KEY="${PILOT_S3_ACCESS_KEY:-pilots}"
export PILOT_S3_SECRET_KEY="${PILOT_S3_SECRET_KEY:-pilots-secret}"
export SSH_OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i ${SSH_KEY}"

say() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }

say "First host: ${IPS[0]}"
"${REPO}/scripts/host-bootstrap.sh" "${IPS[0]}"

FIRST_MESH=$(ssh $SSH_OPTS "root@${IPS[0]}" "/opt/pilots/bin/hostd mesh-addr")
echo "first host's mesh address: ${FIRST_MESH}"

# Joining hosts are pointed at the first host's PUBLIC address; the bootstrap
# script asks it for the key and mesh address itself.
for ip in "${IPS[@]:1}"; do
  say "Joining ${ip}"
  "${REPO}/scripts/host-bootstrap.sh" "$ip" --peer "${IPS[0]}"
done

say "Fleet"
# Every host must see every other host. A host that only sees itself has a
# mesh that came up but gossip that never reached anyone.
for ip in "${IPS[@]}"; do
  COUNT=$(ssh $SSH_OPTS "root@${ip}" \
    "curl -sf http://127.0.0.1:8080/v1/hosts | python3 -c 'import sys,json;print(len(json.load(sys.stdin)))'" 2>/dev/null || echo 0)
  echo "  ${ip} sees ${COUNT} host(s)"
done

{
  echo "PILOT_CORROSION_TOKEN=${PILOT_CORROSION_TOKEN}"
  echo "FIRST_MESH=${FIRST_MESH}"
} >> "$STATE_FILE"

echo
echo "Fleet bootstrapped. Point the e2e battery at any host:"
echo "  PILOTS_E2E=1 PILOTS_E2E_FULL=1 PILOT_API=http://${IPS[0]}:8080 node scripts/e2e.mjs"
