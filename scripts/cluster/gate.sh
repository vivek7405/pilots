#!/usr/bin/env bash
# The Phase 4 gate, run against the local three-node fleet.
#
#   scripts/cluster/gate.sh
#
# Asserts the properties that make this a fleet rather than three machines:
# a host can die and its work comes back somewhere else, with the same URLs
# and no human involved.
set -uo pipefail

cd "$(dirname "$0")"
source ./config.sh
# shellcheck source=/dev/null
source "$STATE_FILE"
REPO="$(cd ../.. && pwd)"
read -ra IPS <<< "$NODE_IPS"

KEY="${PILOT_API_KEY:-pilot_gate_key}"
AUTH="Authorization: Bearer ${KEY}"
SSH="ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -i ${SSH_KEY}"

PASS=0; FAIL=0
ok()   { printf '  \033[32m✓\033[0m %s\n' "$*"; PASS=$((PASS+1)); }
bad()  { printf '  \033[31m✗\033[0m %s\n' "$*"; FAIL=$((FAIL+1)); }
say()  { printf '\n\033[1m== %s\033[0m\n' "$*"; }

api() { # api <ip> <method> <path> [body]
  local ip=$1 method=$2 path=$3 body=${4:-}
  if [ -n "$body" ]; then
    curl -sf -m 180 -X "$method" "http://${ip}:8080${path}" -H "$AUTH" \
      -H 'Content-Type: application/json' -d "$body"
  else
    curl -sf -m 180 -X "$method" "http://${ip}:8080${path}" -H "$AUTH"
  fi
}

jf() { python3 -c "import sys,json;d=json.load(sys.stdin);print(d.get('$1',''))"; }

# Every host authenticates locally against replicated key hashes, so seeding
# the key on one host is enough -- which is itself worth asserting.
say "Seeding an API key on one host"
HASH=$(python3 -c "import hashlib;print(hashlib.sha256('${KEY}'.encode()).hexdigest())")
$SSH "root@${IPS[0]}" "curl -sf -X POST http://127.0.0.1:51002/v1/transactions \
  -H 'Content-Type: application/json' \
  -H \"Authorization: Bearer \$(grep CORROSION_TOKEN /etc/pilots/config | cut -d= -f2)\" \
  --http2-prior-knowledge \
  -d '[{\"query\":\"INSERT INTO api_keys (hash,org_id,scopes,created_at) VALUES (?,?,?,?) ON CONFLICT(hash) DO NOTHING\",\"params\":[\"${HASH}\",\"org-gate\",\"machines\",0]}]'" >/dev/null 2>&1
sleep 3

say "1. Every host sees the whole fleet"
for ip in "${IPS[@]}"; do
  N=$(api "$ip" GET /v1/hosts | python3 -c "import sys,json;print(len(json.load(sys.stdin)))" 2>/dev/null || echo 0)
  [ "$N" = "${#IPS[@]}" ] && ok "${ip} sees ${N} hosts" || bad "${ip} sees ${N} hosts, want ${#IPS[@]}"
done

say "2. A machine created on one host is visible and routable from every host"
M=$(api "${IPS[0]}" POST /v1/machines '{"vcpus":1,"mem_mib":512}')
ID=$(echo "$M" | jf id); NAME=$(echo "$M" | jf name); URL=$(echo "$M" | jf url)
[ -n "$ID" ] && ok "created ${ID} on ${IPS[0]}" || { bad "create failed: $M"; exit 1; }

sleep 3
for ip in "${IPS[@]}"; do
  SEEN=$(api "$ip" GET "/v1/machines/${ID}" | jf id 2>/dev/null)
  [ "$SEEN" = "$ID" ] && ok "${ip} sees the machine" || bad "${ip} does not see it"
done

say "3. Exec through a host that does NOT own the machine"
OWNER=$(api "${IPS[0]}" GET "/v1/machines/${ID}" | jf host_id)
api "${IPS[0]}" POST "/v1/machines/${ID}/exec" '{"cmd":"echo before-failover > /var/tmp/marker"}' >/dev/null
for ip in "${IPS[@]:1}"; do
  OUT=$(api "$ip" POST "/v1/machines/${ID}/exec" '{"cmd":"cat /var/tmp/marker"}' 2>/dev/null | jf stdout)
  [ "$(echo "$OUT" | tr -d '[:space:]')" = "before-failover" ] \
    && ok "${ip} served an exec for a machine owned by ${OWNER}" \
    || bad "${ip} could not serve it (got '${OUT}')"
done

say "4. Give the machine a snapshot in object storage"
# Only a machine with a snapshot can be rescued: its state lives in object
# storage, not on the host that died. A machine created and never suspended
# has nothing to restore FROM, which the rescue loop reports rather than
# retrying forever.
api "${IPS[0]}" POST "/v1/machines/${ID}/suspend" >/dev/null && ok "suspended"
api "${IPS[0]}" POST "/v1/machines/${ID}/wake" >/dev/null && ok "woke"
MEMBUILD=$(api "${IPS[0]}" GET "/v1/machines/${ID}" | jf mem_build_id)
[ -n "$MEMBUILD" ] && ok "machine has a snapshot (${MEMBUILD:0:8})" || bad "no snapshot recorded"

say "5. Hard-kill the host that owns it"
OWNER=$(api "${IPS[0]}" GET "/v1/machines/${ID}" | jf host_id)
OWNER_IP=""
for ip in "${IPS[@]}"; do
  HID=$($SSH "root@${ip}" "grep PILOT_HOST_ID /etc/pilots/config | cut -d= -f2" 2>/dev/null)
  [ "$HID" = "$OWNER" ] && OWNER_IP="$ip"
done
[ -n "$OWNER_IP" ] && ok "owner is ${OWNER} at ${OWNER_IP}" || { bad "could not find the owner"; exit 1; }

SURVIVOR=""
for ip in "${IPS[@]}"; do [ "$ip" != "$OWNER_IP" ] && SURVIVOR="$ip" && break; done

DOMAIN=$(sudo virsh list --name | while read -r d; do
  [ -n "$d" ] && sudo virsh domifaddr "$d" --source lease 2>/dev/null | grep -q "$OWNER_IP" && echo "$d"
done | head -1)
[ -n "$DOMAIN" ] && sudo virsh destroy "$DOMAIN" >/dev/null && ok "destroyed ${DOMAIN} (hard power-off, no shutdown)" \
  || bad "could not destroy the owner's VM"

say "6. The machine returns on a survivor, same URL, no human action"
DEADLINE=$((SECONDS + 180))
NEWOWNER=""
while [ $SECONDS -lt $DEADLINE ]; do
  NEWOWNER=$(api "$SURVIVOR" GET "/v1/machines/${ID}" 2>/dev/null | jf host_id)
  [ -n "$NEWOWNER" ] && [ "$NEWOWNER" != "$OWNER" ] && break
  sleep 5
done
[ -n "$NEWOWNER" ] && [ "$NEWOWNER" != "$OWNER" ] \
  && ok "rescued by ${NEWOWNER} after $((SECONDS - (DEADLINE - 180)))s" \
  || bad "still owned by ${OWNER} after 180s"

NEWURL=$(api "$SURVIVOR" GET "/v1/machines/${ID}" | jf url)
[ "$NEWURL" = "$URL" ] && ok "URL unchanged: ${URL}" || bad "URL changed: ${URL} -> ${NEWURL}"

say "7. The rescued machine serves, with its disk intact"
DEADLINE=$((SECONDS + 120))
OUT=""
while [ $SECONDS -lt $DEADLINE ]; do
  OUT=$(api "$SURVIVOR" POST "/v1/machines/${ID}/exec" '{"cmd":"cat /var/tmp/marker"}' 2>/dev/null | jf stdout)
  [ -n "$OUT" ] && break
  sleep 5
done
[ "$(echo "$OUT" | tr -d '[:space:]')" = "before-failover" ] \
  && ok "the machine came back with what it had written" \
  || bad "exec after rescue returned '${OUT}'"

say "Result"
echo "  ${PASS} passed, ${FAIL} failed"
[ "$FAIL" = 0 ] || exit 1
