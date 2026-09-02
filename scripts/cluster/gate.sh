#!/usr/bin/env bash
# The Phase 4 gate, run against the local three-node fleet.
#
#   scripts/cluster/gate.sh
#
# Asserts the properties that make this a fleet rather than three machines:
# a host can die and its work comes back somewhere else, with the same URLs
# and no human involved -- including, as of Phase 5a, a host that dies with a
# volume mounted and a guest write fsynced to it seconds earlier.
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
  # Live hosts, not rows. A host that was retired -- or destroyed by an
  # earlier run and never brought back -- leaves its row behind, and counting
  # rows fails this on a perfectly healthy fleet.
  N=$(api "$ip" GET /v1/hosts | python3 -c "
import sys, json
print(sum(1 for h in json.load(sys.stdin) if h.get('alive')))" 2>/dev/null || echo 0)
  [ "$N" = "${#IPS[@]}" ] && ok "${ip} sees ${N} hosts" || bad "${ip} sees ${N} hosts, want ${#IPS[@]}"
done

say "2. A machine created on one host is visible and routable from every host"
# auto_stop off for the same reason step 5 turns it off: this machine is used
# by every step below, minutes apart, and a machine that idle-suspends between
# them fails those steps for a reason none of them are about.
M=$(api "${IPS[0]}" POST /v1/machines '{"vcpus":1,"mem_mib":512,"knobs":{"auto_stop":"off"}}')
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

say "5. Two machines in one app reach each other by name, across hosts"
LIVE_IPS=()
for ip in "${IPS[@]}" ${NEW_IP:-}; do
  curl -sf -m 5 "http://${ip}:8080/v1/health" >/dev/null 2>&1 && LIVE_IPS+=("$ip")
done
echo "  live hosts: ${LIVE_IPS[*]}"

APP="gate-shop-$$"
A_IP=""; B_IP=""
if [ "${#LIVE_IPS[@]}" -ge 2 ]; then
  A_IP="${LIVE_IPS[0]}"; B_IP="${LIVE_IPS[1]}"
else
  bad "fewer than two hosts are alive; cross-host discovery cannot be asserted"
fi

WEB_ID=""; DB_ID=""; WEB_NAME=""; DB_NAME=""
if [ -n "$A_IP" ]; then
  # A create lands on the host that serves it, so aiming the two requests at
  # different hosts is how they end up on different hosts -- no placement API
  # and nothing to schedule.
  # auto_stop off, because this step asserts DISCOVERY and not lifecycle.
  #
  # The default is to idle-suspend after a minute, and a suspended machine
  # gives its slot back -- so it has no address and drops out of .internal,
  # correctly. These machines sit idle between assertions while other hosts
  # are being checked, so with the default they vanish mid-step and the
  # failure reads as "the name did not resolve" rather than "the machine you
  # were asking about went to sleep".
  KNOBS='"knobs":{"auto_stop":"off"}'
  WEB=$(api "$A_IP" POST /v1/machines "{\"app\":\"${APP}\",\"vcpus\":1,\"mem_mib\":512,${KNOBS}}")
  DB=$(api "$B_IP" POST /v1/machines \
    "{\"app\":\"${APP}\",\"vcpus\":1,\"mem_mib\":512,${KNOBS},\"cmd\":\"sleep 86400\",\"secret_env\":{\"DB_PASSWORD\":\"gate-secret-$$\"}}")
  WEB_ID=$(echo "$WEB" | jf id); WEB_NAME=$(echo "$WEB" | jf name)
  DB_ID=$(echo "$DB" | jf id);  DB_NAME=$(echo "$DB" | jf name)
  WEB_HOST=$(echo "$WEB" | jf host_id); DB_HOST=$(echo "$DB" | jf host_id)

  if [ -z "$WEB_ID" ] || [ -z "$DB_ID" ]; then
    bad "could not create the pair (web='${WEB}' db='${DB}')"
  elif [ "$WEB_HOST" = "$DB_HOST" ]; then
    bad "both machines landed on ${WEB_HOST}; this asserts nothing about crossing hosts"
  else
    ok "web on ${WEB_HOST}, db on ${DB_HOST}"
  fi
fi

# curl_from <ip> <machine> <url> -> "<http_code> <resolved ip>"
curl_from() {
  api "$1" POST "/v1/machines/$2/exec" \
    "{\"cmd\":\"curl -s -o /dev/null -m 8 -w '%{http_code} %{remote_ip}' $3 || true\",\"user\":\"root\"}" \
    2>/dev/null | jf stdout | tr -d '\n'
}

# curl_until <ip> <machine> <url> -- the same, retried until it works.
#
# Discovery converges rather than existing: a machine that was created a
# moment ago has to reach every host's subscription cache, and the filter and
# the translation rules are rebuilt on a ticker. Asking once and failing is
# asking whether the fleet had already converged, which is not what any of
# these steps are about -- and it is the same reason every other step here
# that depends on the fleet agreeing polls to a deadline.
curl_until() {
  local deadline=$((SECONDS + 90)) out=""
  while [ $SECONDS -lt $deadline ]; do
    out=$(curl_from "$1" "$2" "$3")
    [ "${out%% *}" = "200" ] && { echo "$out"; return; }
    sleep 3
  done
  echo "$out"
}

if [ -n "$WEB_ID" ] && [ -n "$DB_ID" ]; then
  # The listener is the peer's own guest agent: /health needs no credential,
  # so every machine already has one and reaching it proves the whole path --
  # the name resolved, the address translated at both ends, and the filter
  # let it through.
  OUT=$(curl_until "$A_IP" "$WEB_ID" "http://${DB_NAME}.internal:3001/health")
  CODE=${OUT%% *}; PEER=${OUT##* }
  [ "$CODE" = "200" ] && ok "web reached ${DB_NAME}.internal at ${PEER}" \
    || bad "web could not reach ${DB_NAME}.internal (curl said '${OUT}')"
  case "$PEER" in
    fdcd:*) ok "the name resolved to a machine address, not a host one" ;;
    *)      bad "the name resolved to '${PEER}'" ;;
  esac

  OUT=$(curl_until "$B_IP" "$DB_ID" "http://${WEB_NAME}.internal:3001/health")
  [ "${OUT%% *}" = "200" ] && ok "and the reverse direction works too" \
    || bad "db could not reach ${WEB_NAME}.internal (curl said '${OUT}')"
fi

say "6. A guest may not reach a host, and a sealed secret is on no host in the clear"
if [ -n "$WEB_ID" ]; then
  # hostd's internal listener is bearer-authenticated, so a 401 here would
  # prove only that auth was awake. What is being asserted is that the packet
  # never arrives: one leaked API key must not become fleet-wide exec.
  HOST_MESH=$($SSH "root@${B_IP}" "/opt/pilots/bin/hostd mesh-addr" 2>/dev/null | tr -d '\n')
  if [ -z "$HOST_MESH" ]; then
    bad "could not read a peer host's mesh address"
  else
    OUT=$(curl_from "$A_IP" "$WEB_ID" "http://[${HOST_MESH}]:51003/v1/machines")
    case "${OUT%% *}" in
      000) ok "the guest got no reply at all from ${HOST_MESH}:51003" ;;
      401) bad "the guest was answered 401 by hostd at ${HOST_MESH}:51003; auth caught it and the network did not" ;;
      *)   bad "the guest got HTTP ${OUT%% *} from a host address" ;;
    esac
  fi
fi

if [ -n "$DB_ID" ]; then
  # Dumped on a host that does not own the machine, because the point is what
  # GOSSIP carries. The owner's disk holding the value would be a different
  # and smaller claim.
  for ip in "${LIVE_IPS[@]}"; do
    [ "$ip" = "$B_IP" ] && continue
    ROWS=$($SSH "root@${ip}" "sqlite3 /var/lib/pilots/corrosion/store.db \
      \"SELECT COALESCE(env,'')||COALESCE(env_sealed,'') FROM services;\"" 2>/dev/null)
    if echo "$ROWS" | grep -q "gate-secret-$$"; then
      bad "${ip} holds the plaintext secret in a replicated row"
    elif echo "$ROWS" | grep -q "pk1:"; then
      ok "${ip} has the sealed row and cannot read it"
    else
      bad "${ip} has no services row at all; replication did not happen, so this proved nothing"
    fi
    break
  done
fi

say "7. A name still resolves to the right machine after it is rescued"
if [ -n "$WEB_ID" ] && [ -n "$DB_ID" ] && [ "${#LIVE_IPS[@]}" -ge 2 ]; then
  # The db is suspended and woken first: only a machine with a snapshot in
  # object storage can be rescued at all.
  api "$B_IP" POST "/v1/machines/${DB_ID}/suspend" >/dev/null 2>&1
  api "$B_IP" POST "/v1/machines/${DB_ID}/wake" >/dev/null 2>&1

  BEFORE=$(curl_until "$A_IP" "$WEB_ID" "http://${DB_NAME}.internal:3001/health")
  BEFORE_ADDR=${BEFORE##* }

  DOM=$(sudo virsh list --name | while read -r d; do
    [ -n "$d" ] && sudo virsh domifaddr "$d" --source lease 2>/dev/null | grep -q "$B_IP" && echo "$d"
  done | head -1)
  [ -n "$DOM" ] && sudo virsh destroy "$DOM" >/dev/null 2>&1 \
    && ok "destroyed ${DOM}, which was running ${DB_NAME}" \
    || bad "could not destroy the db's host"

  # A rescued machine lands in a new slot on a new host, so its address
  # CHANGES. That is the whole reason answers carry a near-zero TTL, and the
  # assertion is that a client asking again gets the new one.
  START=$SECONDS; AFTER=""
  while [ $((SECONDS - START)) -lt 300 ]; do
    AFTER=$(curl_from "$A_IP" "$WEB_ID" "http://${DB_NAME}.internal:3001/health")
    [ "${AFTER%% *}" = "200" ] && [ "${AFTER##* }" != "$BEFORE_ADDR" ] && break
    sleep 10
  done
  AFTER_ADDR=${AFTER##* }
  if [ "${AFTER%% *}" = "200" ] && [ -n "$AFTER_ADDR" ]; then
    ok "${DB_NAME}.internal now answers at ${AFTER_ADDR} (was ${BEFORE_ADDR}) after $((SECONDS - START))s"
    [ "$AFTER_ADDR" != "$BEFORE_ADDR" ] && ok "the address moved with the machine" \
      || bad "the address did not change; the machine was not actually rescued"
  else
    bad "${DB_NAME}.internal never came back after the rescue (last: '${AFTER}')"
  fi
fi

for id in ${WEB_ID:-} ${DB_ID:-}; do
  for ip in "${LIVE_IPS[@]}"; do
    api "$ip" DELETE "/v1/machines/${id}" >/dev/null 2>&1 && break
  done
done

# Put the host back before the fleet steps start killing their own.
#
# The step above proved its property by destroying one, and everything after
# this needs a full fleet: the steps below hard-kill an owner, kill a host
# mid-request, and kill the host holding a volume, each of which has to find a
# live host to take the work. Leaving this one down means those steps run out
# of fleet and report a broken platform when what actually broke is the gate.
#
# The wait is for the host to be SERVING, not merely booted: a host that has
# not rejoined gossip cannot rescue anything, and a step that starts against
# one fails for a reason that has nothing to do with what it asserts.
if [ -n "${DOM:-}" ]; then
  sudo virsh start "$DOM" >/dev/null 2>&1
  for _ in $(seq 60); do
    curl -sf -m 3 "http://${B_IP}:8080/v1/health" >/dev/null 2>&1 && break
    sleep 2
  done
  curl -sf -m 3 "http://${B_IP}:8080/v1/health" >/dev/null 2>&1 \
    && ok "brought ${DOM} back for the fleet steps" \
    || bad "${DOM} did not come back; the steps below have no fleet to work with"
fi

say "8. Hard-kill the host that owns it"
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

say "9. The machine returns on a survivor, same URL, no human action"
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

say "10. The rescued machine serves, with its disk intact"
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

say "11. One command turns a new IP into a serving host"
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
     PILOT_FLEET_KEY="$PILOT_FLEET_KEY" \
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

say "12. Killing the host a client is mid-request against"
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

say "13. A volume survives the death of the host that had it mounted"
# The two gate lines that a naive test passes without.
#
# A volume is durable because every write goes through to object storage
# before it is acknowledged -- not because it survives a graceful move. So the
# host is hard powered off with the data only ever having been fsynced, and the
# marker has to come back somewhere else. Two defects both pass a gentler
# version of this: a drive left at Firecracker's default cache type, where the
# guest's fsync never reaches the disk at all, and a juicefs mount given
# --writeback, where the acknowledgement happens before the upload.
LIVE_IPS=()
for ip in "${IPS[@]}" ${NEW_IP:-}; do
  [ "$ip" = "$OWNER_IP" ] && continue
  [ "$ip" = "${CUR_IP:-}" ] && continue
  curl -sf -m 5 "http://${ip}:8080/v1/health" >/dev/null 2>&1 && LIVE_IPS+=("$ip")
done

if [ "${#LIVE_IPS[@]}" -lt 2 ]; then
  bad "need two live hosts for the volume failover; have ${#LIVE_IPS[@]}"
else
  VHOST="${LIVE_IPS[0]}"
  VSURVIVOR="${LIVE_IPS[1]}"

  VOL=$(api "$VHOST" POST /v1/volumes '{"name":"gate-volume","size_gib":1,"mount_path":"/data"}')
  VOLID=$(echo "$VOL" | jf id)
  [ -n "$VOLID" ] && ok "created volume ${VOLID} on ${VHOST}" || bad "volume create failed: $VOL"

  if [ -n "$VOLID" ]; then
    VM=$(api "$VHOST" POST /v1/machines "{\"vcpus\":1,\"mem_mib\":512,\"volume\":\"${VOLID}\"}")
    VMID=$(echo "$VM" | jf id); VMURL=$(echo "$VM" | jf url)
    [ -n "$VMID" ] && ok "machine ${VMID} has the volume attached" || bad "create failed: $VM"
  fi

  if [ -n "${VMID:-}" ]; then
    # Read out of the running VMM, not out of what hostd meant to configure.
    # The default cache type does not advertise the VirtIO flush feature, so
    # the guest's fsync returns success with the data in the host page cache
    # and nothing anywhere says so.
    CACHE=$(api "$VHOST" GET "/v1/machines/${VMID}/volume" | jf cache_type)
    [ "$CACHE" = "Writeback" ] \
      && ok "the volume drive is running with cache_type Writeback" \
      || bad "cache_type is '${CACHE}', so a guest fsync does not reach the disk"

    # fsync, not just write. This is the byte the rest of the step is about.
    api "$VHOST" POST "/v1/machines/${VMID}/exec" \
      '{"cmd":"echo durable-marker > /data/marker && dd if=/data/marker of=/data/marker.sync conv=fsync 2>/dev/null && sync","user":"root"}' >/dev/null
    MARK=$(api "$VHOST" POST "/v1/machines/${VMID}/exec" '{"cmd":"cat /data/marker","user":"root"}' | jf stdout)
    [ "$(echo "$MARK" | tr -d '[:space:]')" = "durable-marker" ] \
      && ok "the guest wrote and fsynced to the volume" \
      || bad "the guest could not write to the volume (got '${MARK}')"

    # A machine can only be rescued from a snapshot in object storage, so give
    # it one -- exactly as step 4 does for the ordinary machine.
    # Asserted, not attempted. Both of these used to be `&& ok` with no
    # failing branch, so a suspend that did not happen left the machine with
    # no snapshot and the failure surfaced four assertions later as "the
    # volume lost its data" -- which is not what went wrong.
    api "$VHOST" POST "/v1/machines/${VMID}/suspend" >/dev/null \
      && ok "suspended" || bad "suspend failed; the machine has no snapshot to be rescued from"
    api "$VHOST" POST "/v1/machines/${VMID}/wake" >/dev/null \
      && ok "woke" || bad "wake failed after the suspend"

    VDOM=$(sudo virsh list --name | while read -r d; do
      [ -n "$d" ] && sudo virsh domifaddr "$d" --source lease 2>/dev/null | grep -q "$VHOST" && echo "$d"
    done | head -1)
    [ -n "$VDOM" ] && sudo virsh destroy "$VDOM" >/dev/null 2>&1 \
      && ok "hard powered off ${VDOM}, the host that had the volume mounted" \
      || bad "could not destroy the volume's host"

    VOWNER=""
    START=$SECONDS
    while [ $((SECONDS - START)) -lt 240 ]; do
      VOWNER=$(api "$VSURVIVOR" GET "/v1/machines/${VMID}" 2>/dev/null | jf host_id)
      [ -n "$VOWNER" ] && [ "$VOWNER" != "$(echo "$VM" | jf host_id)" ] && break
      sleep 5
    done
    [ -n "$VOWNER" ] && [ "$VOWNER" != "$(echo "$VM" | jf host_id)" ] \
      && ok "the machine was rescued onto ${VOWNER}" \
      || bad "the volume machine was never rescued"

    NEWVURL=$(api "$VSURVIVOR" GET "/v1/machines/${VMID}" | jf url)
    [ "$NEWVURL" = "$VMURL" ] && ok "URL unchanged: ${VMURL}" \
      || bad "URL changed across the volume failover: ${VMURL} -> ${NEWVURL}"

    # The whole point. The marker was fsynced on a host that then lost power,
    # and it has to be readable on a different one -- which means it reached
    # object storage before the write was acknowledged.
    START=$SECONDS; OUT=""
    while [ $((SECONDS - START)) -lt 180 ]; do
      OUT=$(api "$VSURVIVOR" POST "/v1/machines/${VMID}/exec" \
        '{"cmd":"cat /data/marker","user":"root"}' 2>/dev/null | jf stdout)
      [ -n "$(echo "$OUT" | tr -d '[:space:]')" ] && break
      sleep 5
    done
    [ "$(echo "$OUT" | tr -d '[:space:]')" = "durable-marker" ] \
      && ok "the fsynced write survived a hard host kill and came back elsewhere" \
      || bad "the volume lost its data across the failover (got '${OUT}')"

    # And it is a volume on the new host too, not the ephemeral root pretending
    # to be one. A machine whose volume failed to mount reads and writes /data
    # perfectly well -- on a disk that disappears with it.
    VDEV=$(api "$VSURVIVOR" POST "/v1/machines/${VMID}/exec" \
      '{"cmd":"awk \u0027$2 == \"/data\" { print $1 }\u0027 /proc/self/mounts","user":"root"}' 2>/dev/null | jf stdout)
    echo "$VDEV" | grep -q vdb \
      && ok "/data is the volume drive on the rescuing host" \
      || bad "/data is backed by '${VDEV}', not the volume"
  fi
fi

# ---------------------------------------------------------------------------
# Phase 5b. Everything below is about machines finding and reaching each other,
# which is the half of the design that has no reference implementation: the
# DNS server ports cleanly from uncloud and the addressing does not, because a
# cluster-wide allocator is exactly what this fleet has no room for.
#
# It runs after the failover steps deliberately. The rig is at its most
# awkward here -- two hosts powered off, one added mid-run -- and a name that
# only resolves on a pristine fleet is not the property being claimed.
# ---------------------------------------------------------------------------

say "Result"
echo "  ${PASS} passed, ${FAIL} failed"
echo
echo "This run left the rig changed on purpose:"
echo "  - three hosts are powered off (steps 8, 12 and 13); sudo virsh start <domain>"
echo "  - the fleet is one host bigger (step 11), and cluster.env records it,"
echo "    so the next run adds another. cluster-down.sh resets it."
[ "$FAIL" = 0 ] || exit 1
