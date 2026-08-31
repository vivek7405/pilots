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
# Read from the row rather than the API: build ids are internal and the API
# deliberately does not expose them.
MEMBUILD=$($SSH "root@${IPS[0]}" "sqlite3 /var/lib/pilots/corrosion/store.db \
  \"SELECT mem_build_id FROM machines WHERE id='${ID}';\"" 2>/dev/null | tr -d '[:space:]')
[ -n "$MEMBUILD" ] && ok "machine has a snapshot (${MEMBUILD:0:8})" \
  || bad "no snapshot recorded; it could not be rescued"

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

say "8. One command turns a new IP into a serving host"
# "Add a host = give an IP" is the claim. On real hardware the machine is
# already racked and already has an address, so provisioning one more VM is
# the local stand-in for that and is not part of what is being asserted. What
# IS asserted is everything after it: one command, and the fleet is bigger.
#
# NEW_ONLY matters here. A host is deliberately powered off at this point, and
# a plain cluster-up would start it back up and quietly undo step 5.
NEW_N=$(( ${#IPS[@]} + 1 ))
if NODES=$NEW_N NEW_ONLY=1 ./cluster-up.sh >/dev/null 2>&1; then
  NEW_IP=$(sed -n 's/^NODE_IPS="\(.*\)"$/\1/p' "$STATE_FILE" | tail -1 | awk -v n="$NEW_N" '{print $n}')
else
  NEW_IP=""
fi
[ -n "$NEW_IP" ] && ok "a new machine exists at ${NEW_IP}" || bad "could not provision one"

if [ -n "$NEW_IP" ]; then
  # The one command. It is pointed at a SURVIVOR, not at the first host --
  # joining must not depend on any particular host being alive, and the host
  # this fleet started from is currently powered off.
  if PILOT_CORROSION_TOKEN="$PILOT_CORROSION_TOKEN" \
     PILOT_AGENT_TOKEN_SECRET="$PILOT_AGENT_TOKEN_SECRET" \
     PILOT_S3_ENDPOINT="${PILOT_S3_ENDPOINT:-http://${NET_SUBNET}.1:9000}" \
     PILOT_S3_BUCKET="${PILOT_S3_BUCKET:-pilots}" \
     PILOT_S3_ACCESS_KEY="${PILOT_S3_ACCESS_KEY:-pilots}" \
     PILOT_S3_SECRET_KEY="${PILOT_S3_SECRET_KEY:-pilots-secret}" \
     "${REPO}/scripts/host-bootstrap.sh" "$NEW_IP" --peer "$SURVIVOR" >/dev/null 2>&1; then
    ok "host-bootstrap.sh ${NEW_IP} --peer ${SURVIVOR} succeeded"
  else
    bad "the one command failed"
  fi

  # Live hosts only: the one killed in step 5 is still down on purpose, so the
  # fleet the new host should see is the survivors plus itself.
  LIVE=$(( ${#IPS[@]} - 1 + 1 ))

  # Poll rather than assert once. The checklist asks for schedulable and
  # serving "within minutes", not instantly: a host that has just joined still
  # has to exchange gossip and heartbeat once before anyone counts it alive.
  # Asserting the moment the bootstrap returns measures convergence latency
  # and calls it a failure.
  live_seen() { # live_seen <ip>
    api "$1" GET /v1/hosts 2>/dev/null | python3 -c "
import sys, json
print(sum(1 for h in json.load(sys.stdin) if h.get('alive')))" 2>/dev/null || echo 0
  }

  START=$SECONDS; N=0
  while [ $((SECONDS - START)) -lt 120 ]; do
    N=$(live_seen "$NEW_IP"); [ "$N" = "$LIVE" ] && break
    sleep 5
  done
  [ "$N" = "$LIVE" ] && ok "the new host sees ${N} live hosts after $((SECONDS - START))s" \
    || bad "the new host sees ${N} live hosts, want ${LIVE}"

  START=$SECONDS; N=0
  while [ $((SECONDS - START)) -lt 120 ]; do
    N=$(live_seen "$SURVIVOR"); [ "$N" = "$LIVE" ] && break
    sleep 5
  done
  [ "$N" = "$LIVE" ] && ok "the existing hosts see it too" \
    || bad "an existing host sees ${N} live hosts, want ${LIVE}"

  # Serving, not merely present. A host that joined gossip but cannot route is
  # in the fleet's tables and useless to a client.
  START=$SECONDS; OUT=""
  while [ $((SECONDS - START)) -lt 120 ]; do
    OUT=$(api "$NEW_IP" POST "/v1/machines/${ID}/exec" '{"cmd":"cat /var/tmp/marker"}' 2>/dev/null | jf stdout)
    [ -n "$(echo "$OUT" | tr -d '[:space:]')" ] && break
    sleep 5
  done
  [ "$(echo "$OUT" | tr -d '[:space:]')" = "before-failover" ] \
    && ok "the new host served a request for a machine it does not own" \
    || bad "the new host could not serve it (got '${OUT}')"
fi

say "9. Killing the host a client is mid-request against"
# The client is talking to the machine's owner when that host dies. Every
# workload name resolves to every host, so the client's retry lands somewhere
# else -- and that host has to be able to finish the job, which means claiming
# a machine whose owner is gone and restoring it from object storage.
#
# This is a different path from step 6: there, a background loop noticed a
# dead host with nobody waiting. Here a client is on the line the whole time.
CUR_OWNER=$(api "$SURVIVOR" GET "/v1/machines/${ID}" | jf host_id)
CUR_IP=""
for ip in "${IPS[@]}" ${NEW_IP:-}; do
  HID=$($SSH "root@${ip}" "grep PILOT_HOST_ID /etc/pilots/config | cut -d= -f2" 2>/dev/null)
  [ "$HID" = "$CUR_OWNER" ] && CUR_IP="$ip"
done

NEXT=""
for ip in "${IPS[@]}" ${NEW_IP:-}; do
  [ "$ip" != "$CUR_IP" ] && [ "$ip" != "$OWNER_IP" ] && NEXT="$ip" && break
done

if [ -z "$CUR_IP" ] || [ -z "$NEXT" ]; then
  bad "could not find a live owner and a host to retry against"
else
  # In flight, and slow enough that the host dies underneath it rather than
  # after it. The reply never arrives; that is the point.
  api "$CUR_IP" POST "/v1/machines/${ID}/exec" '{"cmd":"sleep 45; echo done"}' >/dev/null 2>&1 &
  INFLIGHT=$!
  sleep 5

  DOM=$(sudo virsh list --name | while read -r d; do
    [ -n "$d" ] && sudo virsh domifaddr "$d" --source lease 2>/dev/null | grep -q "$CUR_IP" && echo "$d"
  done | head -1)
  [ -n "$DOM" ] && sudo virsh destroy "$DOM" >/dev/null 2>&1 \
    && ok "destroyed ${DOM} with a request in flight against it" \
    || bad "could not destroy the owner mid-request"

  # The in-flight call is now talking to a powered-off machine. Nothing can
  # make that return promptly -- the packets go nowhere and there is no RST to
  # read -- so the client's own timeout is its business, not the fleet's. Stop
  # waiting on it, the way a client with a retry would.
  kill "$INFLIGHT" 2>/dev/null
  wait "$INFLIGHT" 2>/dev/null

  # The retry. It has to wait out the fleet declaring the owner dead before
  # anyone may claim the machine, so this is slow by design, not by accident.
  START=$SECONDS
  OUT=""
  while [ $((SECONDS - START)) -lt 240 ]; do
    OUT=$(api "$NEXT" POST "/v1/machines/${ID}/exec" '{"cmd":"cat /var/tmp/marker"}' 2>/dev/null | jf stdout)
    [ -n "$(echo "$OUT" | tr -d '[:space:]')" ] && break
    sleep 5
  done
  [ "$(echo "$OUT" | tr -d '[:space:]')" = "before-failover" ] \
    && ok "the retry was served by ${NEXT} after $((SECONDS - START))s, same machine, same disk" \
    || bad "the retry never succeeded (got '${OUT}')"
fi

say "Result"
echo "  ${PASS} passed, ${FAIL} failed"
echo
echo "This run left the rig changed on purpose:"
echo "  - two hosts are powered off (steps 5 and 9); sudo virsh start <domain>"
echo "  - the fleet is one host bigger (step 8), and cluster.env records it,"
echo "    so the next run adds another. cluster-down.sh resets it."
[ "$FAIL" = 0 ] || exit 1
