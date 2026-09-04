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

# Every host authenticates locally against replicated key hashes, so minting
# the key on ONE host is enough -- which is itself worth asserting.
#
# `hostd bootstrap-key` rather than a hand-written corrosion transaction: the
# key shape, the hash and the scopes then have exactly one implementation, and
# a change to any of them cannot leave this script writing rows hostd no
# longer recognises. It prints the plaintext and nothing else, and mints an
# admin key, which the gate needs because it drives routes from every scope.
say "Minting an API key on one host"
KEY="${PILOT_API_KEY:-$($SSH "root@${IPS[0]}" /opt/pilots/bin/hostd bootstrap-key 2>/dev/null | tail -1)}"
AUTH="Authorization: Bearer ${KEY}"
case "$KEY" in
  pilot_*) ok "minted a key on ${IPS[0]}" ;;
  *) bad "could not mint a key on ${IPS[0]}"; exit 1 ;;
esac
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

say "1b. Every host answers on the API hostname and reports a replica version"
# The API hostname sits under the workload wildcard, so nothing routes to it
# unless dispatch claims it before the suffix check. There is no DNS and no
# TLS on this cluster, so the hostname is asserted with a Host: header against
# the IP -- which is exactly what the check reads anyway.
WD="${PILOT_WORKLOAD_DOMAIN:-pilotrun.app}"
VERSIONS=()
for ip in "${IPS[@]}"; do
  H=$(curl -sf -m 5 -H "Host: api.${WD}" "http://${ip}:8080/v1/health" 2>/dev/null)
  [ "$(echo "$H" | jf ok)" = "True" ] \
    && ok "${ip} answers on api.${WD}" \
    || bad "${ip} does not answer on api.${WD}: ${H:-no response}"

  # The sum of the replica's version vector. Zero means the local corrosion
  # agent is not answering, which is a broken host wearing a healthy 200.
  V=$(echo "$H" | jf store_version)
  case "$V" in
    ''|*[!0-9]*) bad "${ip} reports store_version ${V:-empty}, want an integer" ;;
    0) bad "${ip} reports store_version 0, so its replica is not readable" ;;
    *) ok "${ip} has applied ${V} changes"; VERSIONS+=("$V") ;;
  esac
done

if [ "${#VERSIONS[@]}" = "${#IPS[@]}" ]; then
  # Each host may be one heartbeat behind every other host at the instant of
  # the read, so the fleet size is the allowance. A wider spread than that is
  # replication falling behind, not sampling.
  MAXV=$(printf '%s\n' "${VERSIONS[@]}" | sort -n | tail -1)
  MINV=$(printf '%s\n' "${VERSIONS[@]}" | sort -n | head -1)
  SPREAD=$((MAXV - MINV))
  [ "$SPREAD" -le "${#IPS[@]}" ] \
    && ok "replica versions are within ${SPREAD} of each other (${VERSIONS[*]})" \
    || bad "replica versions span ${SPREAD}, want at most ${#IPS[@]} (${VERSIONS[*]})"
fi

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

# The added host is this run's, not the fleet's.
#
# cluster-up.sh records NODES and NODE_IPS in the state file, so growing the
# fleet here grows the DEFINITION of the fleet permanently. The next run then
# starts by trying to bootstrap a host that no longer exists, and during Phase
# 5 that left NODES=4 with an address that had no VM behind it -- one whole
# run hung in bootstrap and never reached a single assertion. Restored on the
# way out, however this script exits.
restore_fleet_definition() {
  [ -n "${FLEET_NODES:-}" ] || return 0
  sed -i "s/^NODES=.*/NODES=${FLEET_NODES}/; s|^NODE_IPS=.*|NODE_IPS=\"${FLEET_NODE_IPS}\"|" \
    "$STATE_FILE" 2>/dev/null || true

  # And REMOVE the host this run added, not just its entry in the state file.
  #
  # Restoring the definition without destroying the domain leaves a VM nobody
  # counts: it keeps its old state, rejoins the mesh on the next boot, and
  # gossips rows for machines that no longer exist. That stray host was the
  # real cause of a long run of Phase 5 failures first attributed to the code,
  # and it recurs on EVERY gate run because adding a host is one of the
  # assertions. Leaving it to whoever runs the gate next to notice is not a
  # fix, it is a note.
  # Literal sudo, like every other virsh call in this file. $SUDO is
  # cluster-up.sh's convention and is EMPTY here, so the first version of this
  # ran virsh unprivileged, failed the dominfo probe, and skipped the cleanup
  # silently -- the gate passed 45/0 and left the host behind anyway.
  local added="${NODE_PREFIX}-$(( FLEET_NODES + 1 ))"
  if sudo virsh dominfo "$added" >/dev/null 2>&1; then
    sudo virsh destroy "$added" >/dev/null 2>&1 || true
    sudo virsh undefine "$added" --remove-all-storage >/dev/null 2>&1 || true
    echo "  cleaned up ${added}, the host this run added"
  fi
}
FLEET_NODES="$NODES"
FLEET_NODE_IPS="${IPS[*]}"
trap restore_fleet_definition EXIT

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

  # Minted on the host step 13 is about to power off, so 13b can prove the key
  # outlives it. Minting it here rather than after the kill is the whole point:
  # a key created on a host that is still alive proves nothing about
  # replication.
  MINTED=$(api "$VHOST" POST /v1/api-keys '{"org_id":"gate-org","scopes":["machines"]}' 2>/dev/null)
  MINTED_KEY=$(echo "$MINTED" | jf key)
  case "$MINTED_KEY" in
    pilot_*) ok "minted a key on ${VHOST}, the host about to be killed" ;;
    *) bad "could not mint a key on ${VHOST}: ${MINTED}" ;;
  esac

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

say "13b. A key minted on a dead host still authenticates on every survivor"
# The architecture's central claim, asserted where it can actually fail.
#
# The dashboard mints keys through POST /v1/api-keys and NEVER verifies one:
# each host answers from its own replica of the hash. So a key is only as
# available as replication makes it, and the way to prove that is to mint one
# on a host and then use it after that host is gone.
#
# Step 13 has just hard powered off ${VHOST}. A key minted THERE, before the
# power cut, is the interesting case: if minting wrote only to the minting
# host, every call below is a 401 and the dashboard becomes a thing the fleet
# depends on, which is exactly what the design forbids.
if [ -n "${VHOST:-}" ] && [ -n "${MINTED_KEY:-}" ]; then
  MINTED_AUTH="Authorization: Bearer ${MINTED_KEY}"
  SURVIVORS=()
  for ip in "${IPS[@]}" ${NEW_IP:-}; do
    [ "$ip" = "$VHOST" ] && continue
    curl -sf -m 5 "http://${ip}:8080/v1/health" >/dev/null 2>&1 && SURVIVORS+=("$ip")
  done

  if [ "${#SURVIVORS[@]}" -eq 0 ]; then
    bad "no survivor left to check the minted key against"
  else
    for ip in "${SURVIVORS[@]}"; do
      curl -sf -m 15 "http://${ip}:8080/v1/machines" -H "$MINTED_AUTH" >/dev/null 2>&1 \
        && ok "${ip} authenticates the key minted on the now-dead ${VHOST}" \
        || bad "${ip} rejected a key minted on ${VHOST}; auth depends on the minting host"
    done

    # And a garbage key is still refused, so the assertion above is not just
    # "this host authenticates everything".
    curl -sf -m 15 "http://${SURVIVORS[0]}:8080/v1/machines" \
      -H "Authorization: Bearer pilot_definitely_not_a_real_key" >/dev/null 2>&1 \
      && bad "${SURVIVORS[0]} accepted a key that was never minted" \
      || ok "${SURVIVORS[0]} still refuses a key that was never minted"
  fi
else
  bad "step 13 left no powered-off host or no minted key to check"
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

# ---------------------------------------------------------------------------
# Phase 6e. Hostility: the incident classes the predecessor paid for.
#
# Everything below needs a HOST SHELL. /sys/block/nbdN/pid, a process's
# D-state, cgroup memory.events, /proc/<pid>/fd and `kill -9 hostd` are all
# invisible to the public API, which is why they live here and not in
# scripts/e2e.mjs -- that battery's rule is that it drives the API only. The
# API-visible half of the same classes (churn, egress, capacity, quota) is in
# e2e.mjs's hostilityAssertions().
#
# These run after section 13 deliberately, for the same reason 5b does: the rig
# is at its most awkward here, with hosts powered off and one added mid-run. A
# property that holds only on a pristine fleet is not the property being
# claimed, so the live hosts are discovered rather than assumed.
#
# The NBD fault is LAST (section 19) rather than in the middle. It leaves a
# device attached to a dead server and a Firecracker in D-state, which is
# exactly the wedge it exists to prove and exactly the state no later section
# can run on: the host needs a reboot before it is a host again. Putting the
# negative control anywhere but the end would mean either a second live host
# for every section after it, or a rig that is broken for the rest of the run.
# ---------------------------------------------------------------------------

HOSTILE_IPS=()
for ip in "${IPS[@]}" ${NEW_IP:-}; do
  curl -sf -m 5 "http://${ip}:8080/v1/health" >/dev/null 2>&1 && HOSTILE_IPS+=("$ip")
done
H_IP="${HOSTILE_IPS[0]:-}"

# nbd_attached lists the NBD devices with a live server, sorted so the sets can
# be diffed. A pid file that exists but is empty is a device nobody is serving.
nbd_attached() {
  $SSH "root@$1" 'for f in /sys/block/nbd*/pid; do [ -s "$f" ] && basename "$(dirname "$f")"; done' \
    2>/dev/null | sort
}

# slice_of finds a machine's cgroup v2 slice without hard-coding the layout.
# The jailer builds the path from --parent-cgroup, the exec file's name and
# --id, so it is pilots/firecracker/<id> on a current Firecracker and pilots/<id>
# on older ones. A `find` is right either way, and a wrong hard-coded path
# would read as "the limit is missing" rather than "the path moved".
slice_of() {
  $SSH "root@$1" "find /sys/fs/cgroup/pilots -maxdepth 3 -type d -name '$2' 2>/dev/null | head -1" \
    2>/dev/null | tr -d '[:space:]'
}

# fc_pid reads the Firecracker pid out of the machine's own slice. pgrep will
# not do it: the jailer execve()s Firecracker over itself, so the --id that
# named the machine is gone from the command line by the time it matters.
fc_pid() {
  local slice; slice=$(slice_of "$1" "$2")
  [ -z "$slice" ] && return
  $SSH "root@$1" "head -1 ${slice}/cgroup.procs" 2>/dev/null | tr -d '[:space:]'
}

# host_counts prints the three host-side resources a machine owns, so a leak is
# a diff rather than a judgement call.
#
# The taps are counted by their veth half. TapName is "vmnet" for every machine
# and it lives INSIDE the namespace, so it is invisible from the root one --
# counting "vmnet" on the host returns zero on a perfectly healthy fleet and on
# a badly leaking one alike. veth-<idx> is the half that is in the root
# namespace and it is one per slot.
host_counts() {
  $SSH "root@$1" 'echo "$(ls /var/run/netns 2>/dev/null | wc -l) $(ip -o link 2>/dev/null | grep -c "veth-") $(for f in /sys/block/nbd*/pid; do [ -s "$f" ] && echo x; done | wc -l)"' \
    2>/dev/null | tr -d '\n'
}

# live_machines_on counts the machines a host is RUNNING a Firecracker for.
#
# 'running', not 'not destroyed': a suspended machine is a perfectly healthy
# row whose VMM is gone -- giving the slot back is the whole point of suspend
# -- so counting it would report a divergence on a host that has none.
live_machines_on() {
  local hid; hid=$(curl -sf -m 5 "http://$1:8080/v1/health" | jf host_id)
  api "$1" GET /v1/machines | python3 -c "
import sys, json
rows = json.load(sys.stdin)
print(sum(1 for m in rows if m.get('host_id') == '$hid' and m.get('state') == 'running'))" 2>/dev/null || echo -1
}

# machine_ids_on lists the ids a host still owns, sorted so two snapshots can
# be diffed with comm.
machine_ids_on() {
  local hid; hid=$(curl -sf -m 5 "http://$1:8080/v1/health" | jf host_id)
  api "$1" GET /v1/machines | python3 -c "
import sys, json
for m in json.load(sys.stdin):
    if m.get('host_id') == '$hid' and m.get('state') != 'destroyed':
        print(m['id'])" 2>/dev/null | sort
}

# wait_serving polls a host's health until it answers or the deadline passes.
wait_serving() {
  local ip=$1 limit=${2:-120} start=$SECONDS
  while [ $((SECONDS - start)) -lt "$limit" ]; do
    curl -sf -m 5 "http://${ip}:8080/v1/health" >/dev/null 2>&1 && return 0
    sleep 2
  done
  return 1
}

# exec_ms times one exec through the API, in milliseconds. H4 needs a measured
# baseline rather than a hard-coded figure: host speed varies, and a literal
# millisecond budget here would only ever measure the rig.
exec_ms() {
  local start end
  start=$(date +%s%N)
  api "$1" POST "/v1/machines/$2/exec" '{"cmd":"true","user":"root"}' >/dev/null 2>&1
  end=$(date +%s%N)
  echo $(( (end - start) / 1000000 ))
}

say "14. A destroy frees its NBD device without wedging the host"
# ARCHITECTURE.md:507-509 and the predecessor's nbd-rootfs port both record the
# same failure: a handler blocked in NBD_DO_IT never reaches its own cleanup,
# so a SIGTERM that arrives before the disconnect ioctl leaves /dev/nbdN
# attached to a server that is gone. Firecracker then blocks in D-state on it,
# the device is unusable, and only a reboot clears it. Process.Stop issues the
# disconnect FIRST and waits for /sys/block/nbdN/pid to empty before the device
# goes back in the pool; this is that ordering, observed from outside.
H1_ID=""; H1B_ID=""; FREED=""
if [ -z "$H_IP" ]; then
  bad "no host is alive; the hostility sections cannot run"
else
  echo "  live hosts: ${HOSTILE_IPS[*]}"
  H1=$(api "$H_IP" POST /v1/machines '{"vcpus":1,"mem_mib":512,"knobs":{"auto_stop":"off"}}')
  H1_ID=$(echo "$H1" | jf id)
  [ -n "$H1_ID" ] && ok "created ${H1_ID} on ${H_IP}" || bad "create failed: $H1"
fi

if [ -n "$H1_ID" ]; then
  H1_FCPID=$(fc_pid "$H_IP" "$H1_ID")
  [ -n "$H1_FCPID" ] && ok "its Firecracker is pid ${H1_FCPID}" \
    || bad "could not find the Firecracker process for ${H1_ID}"

  BEFORE_NBD=$(nbd_attached "$H_IP")
  if [ -z "$BEFORE_NBD" ]; then
    bad "no NBD device is attached, so freeing one cannot be asserted"
  else
    ok "attached before the destroy: $(echo "$BEFORE_NBD" | tr '\n' ' ')"
  fi

  api "$H_IP" DELETE "/v1/machines/${H1_ID}" >/dev/null \
    && ok "destroyed ${H1_ID}" || bad "destroy of ${H1_ID} failed"

  # Five seconds, polled. The disconnect is synchronous but the kernel's own
  # teardown is not, and WaitDetached is what the pool waits on.
  for _ in $(seq 25); do
    AFTER_NBD=$(nbd_attached "$H_IP")
    FREED=$(comm -23 <(echo "$BEFORE_NBD") <(echo "$AFTER_NBD"))
    [ -n "$FREED" ] && break
    sleep 0.2
  done
  [ -n "$FREED" ] \
    && ok "$(echo "$FREED" | tr '\n' ' ')freed within 5s of the destroy" \
    || bad "no NBD device was freed within 5s; the disconnect ioctl did not run"

  if [ -n "$H1_FCPID" ]; then
    STAT=$($SSH "root@$H_IP" "ps -o stat= -p ${H1_FCPID} 2>/dev/null" 2>/dev/null | tr -d '[:space:]')
    case "$STAT" in
      "")  ok "the Firecracker process is gone" ;;
      *D*) bad "the Firecracker process is in uninterruptible sleep ('${STAT}'); the host is wedged" ;;
      *)   ok "the Firecracker process is not in D-state (state '${STAT}')" ;;
    esac
  fi

  # A device that was freed but not reusable is the same outage one create
  # later, so the assertion runs until the device is back in service.
  H1B=$(api "$H_IP" POST /v1/machines '{"vcpus":1,"mem_mib":512,"knobs":{"auto_stop":"off"}}')
  H1B_ID=$(echo "$H1B" | jf id)
  if [ -z "$H1B_ID" ]; then
    bad "a create after the destroy failed: $H1B"
  else
    REUSED=$(nbd_attached "$H_IP" | comm -12 - <(echo "$FREED"))
    [ -n "$REUSED" ] && ok "the later create reused $(echo "$REUSED" | tr '\n' ' ')" \
      || bad "the freed device was not reused; it is out of the pool"
    OUT=$(api "$H_IP" POST "/v1/machines/${H1B_ID}/exec" '{"cmd":"echo nbd-alive","user":"root"}' 2>/dev/null | jf stdout)
    [ "$(echo "$OUT" | tr -d '[:space:]')" = "nbd-alive" ] \
      && ok "and the machine on it serves" || bad "the machine on the reused device does not serve (got '${OUT}')"
    api "$H_IP" DELETE "/v1/machines/${H1B_ID}" >/dev/null 2>&1
  fi
fi

say "15. A hundred create/destroy cycles return the host to its baseline"
# The e2e battery runs the same loop and asserts the host can still serve
# afterwards. This is the half it cannot see: whether the namespaces, the veth
# halves and the NBD devices actually went back.
#
# The EBUSY retry in netns/teardown.go is the mechanism under test. A destroy
# races the death of the Firecracker that held the namespace open, so the
# delete returns EBUSY and has to be retried; a destroy that gives up leaves a
# stale namespace and the next create on that slot fails with "file exists".
CHURN_N=${GATE_CHURN_N:-100}
if [ -n "$H_IP" ]; then
  BASE_COUNTS=$(host_counts "$H_IP")
  # Read on the NODE, not here: journalctl --since is interpreted by the node's
  # clock, and any skew between the two silently moves the window.
  SINCE=$($SSH "root@$H_IP" "date '+%Y-%m-%d %H:%M:%S'" 2>/dev/null | tr -d '\n')
  ok "baseline on ${H_IP}: netns/veth/nbd = ${BASE_COUNTS}"

  CHURN_FAIL=0
  for i in $(seq "$CHURN_N"); do
    CM=$(api "$H_IP" POST /v1/machines '{"vcpus":1,"mem_mib":512}')
    CID=$(echo "$CM" | jf id)
    if [ -z "$CID" ]; then bad "cycle ${i}: create failed: $CM"; CHURN_FAIL=1; break; fi
    api "$H_IP" DELETE "/v1/machines/${CID}" >/dev/null 2>&1 \
      || { bad "cycle ${i}: destroy of ${CID} failed"; CHURN_FAIL=1; break; }
  done
  [ "$CHURN_FAIL" = 0 ] && ok "${CHURN_N} create/destroy cycles completed"

  # Teardown is asynchronous at the edges, so the counts are given a moment to
  # settle rather than read the instant the last delete returned.
  AFTER_COUNTS=""
  for _ in $(seq 30); do
    AFTER_COUNTS=$(host_counts "$H_IP")
    [ "$AFTER_COUNTS" = "$BASE_COUNTS" ] && break
    sleep 2
  done
  [ "$AFTER_COUNTS" = "$BASE_COUNTS" ] \
    && ok "netns, veth halves and NBD devices are all back to baseline (${AFTER_COUNTS})" \
    || bad "the host did not return to baseline: ${BASE_COUNTS} -> ${AFTER_COUNTS}"

  # The retry itself is silent by design -- deleteNamedNS logs nothing until it
  # has exhausted all ten attempts -- so what is asserted is the observable
  # half: across a hundred races, not one teardown surfaced as an error.
  ERRS=$($SSH "root@$H_IP" "journalctl -u hostd --since '${SINCE}' --no-pager 2>/dev/null | grep -c -e 'still EBUSY after' -e 'netns: delete'" 2>/dev/null | tr -d '[:space:]')
  [ "${ERRS:-0}" = "0" ] \
    && ok "no teardown error in the journal across ${CHURN_N} cycles" \
    || bad "${ERRS} teardown error(s) in the journal; an EBUSY retry gave up"
fi

say "16. A guest's memory and fork bombs stay inside its own cgroup slice"
# The jailer puts every machine in a cgroup v2 slice with memory.max, cpu.max
# and pids.max derived per machine (memory.max is mem_mib + 128 MiB). This is
# the assertion multi-tenancy rests on, and it is worthless against an idle
# neighbour: B is created first, measured under the same load the assertion
# will re-measure it under, and only then is A allowed to misbehave.
if [ -n "$H_IP" ]; then
  NB=$(api "$H_IP" POST /v1/machines '{"vcpus":1,"mem_mib":512,"knobs":{"auto_stop":"off"}}')
  NB_ID=$(echo "$NB" | jf id)
  [ -n "$NB_ID" ] && ok "neighbour ${NB_ID} is up" || bad "could not create the neighbour: $NB"

  BASE_MS=0
  if [ -n "$NB_ID" ]; then
    SAMPLES=$(for _ in $(seq 5); do exec_ms "$H_IP" "$NB_ID"; done | sort -n)
    BASE_MS=$(echo "$SAMPLES" | sed -n '3p')
    [ "${BASE_MS:-0}" -gt 0 ] \
      && ok "the neighbour's median exec latency is ${BASE_MS}ms" \
      || bad "could not measure the neighbour's baseline latency (samples: $(echo "$SAMPLES" | tr '\n' ' '))"
  fi

  BOMB=$(api "$H_IP" POST /v1/machines '{"vcpus":1,"mem_mib":256,"knobs":{"auto_stop":"off"}}')
  BOMB_ID=$(echo "$BOMB" | jf id)
  [ -n "$BOMB_ID" ] && ok "the hostile machine ${BOMB_ID} is up" || bad "could not create it: $BOMB"

  if [ -n "$BOMB_ID" ] && [ -n "$NB_ID" ]; then
    BOMB_SLICE=$(slice_of "$H_IP" "$BOMB_ID")
    [ -n "$BOMB_SLICE" ] && ok "its slice is ${BOMB_SLICE}" \
      || bad "the machine has no cgroup slice; nothing is limiting it"

    if [ -n "$BOMB_SLICE" ]; then
      MEM_MAX=$($SSH "root@$H_IP" "cat ${BOMB_SLICE}/memory.max" 2>/dev/null | tr -d '[:space:]')
      PIDS_MAX=$($SSH "root@$H_IP" "cat ${BOMB_SLICE}/pids.max" 2>/dev/null | tr -d '[:space:]')
      [ -n "$MEM_MAX" ] && [ "$MEM_MAX" != "max" ] \
        && ok "memory.max is ${MEM_MAX} and pids.max is ${PIDS_MAX}" \
        || bad "memory.max is '${MEM_MAX}'; the slice has no memory limit at all"

      # All three bombs, back to back, each bounded so the step ends. The
      # memory bomb is the one that reaches the host: a guest touching every
      # page it has drives the Firecracker process's RSS at memory.max.
      # sample_while polls the slice for as long as the background exec whose
      # pid it is given is still running, so every peak below covers the bomb
      # it is reported against. Sampling only the memory bomb and then quoting
      # the pids peak against the FORK bomb -- which had not been launched yet
      # -- reports a number from the wrong window.
      MEM_PEAK=0; PIDS_PEAK=0
      sample_while() {
        while kill -0 "$1" 2>/dev/null; do
          READING=$($SSH "root@$H_IP" "cat ${BOMB_SLICE}/memory.current ${BOMB_SLICE}/pids.current 2>/dev/null | tr '\n' ' '" 2>/dev/null)
          CUR_MEM=$(echo "$READING" | awk '{print $1}'); CUR_PIDS=$(echo "$READING" | awk '{print $2}')
          [ -n "$CUR_MEM" ] && [ "$CUR_MEM" -gt "$MEM_PEAK" ] 2>/dev/null && MEM_PEAK=$CUR_MEM
          [ -n "$CUR_PIDS" ] && [ "$CUR_PIDS" -gt "$PIDS_PEAK" ] 2>/dev/null && PIDS_PEAK=$CUR_PIDS
          sleep 1
        done
        wait "$1" 2>/dev/null
      }

      api "$H_IP" POST "/v1/machines/${BOMB_ID}/exec" \
        '{"cmd":"timeout 30 sh -c \u0027tail /dev/zero\u0027 >/dev/null 2>&1; exit 0","user":"root"}' >/dev/null 2>&1 &
      sample_while $!

      api "$H_IP" POST "/v1/machines/${BOMB_ID}/exec" \
        '{"cmd":"timeout 15 sh -c \u0027bomb(){ bomb|bomb & }; bomb\u0027 >/dev/null 2>&1; exit 0","user":"root"}' >/dev/null 2>&1 &
      sample_while $!

      api "$H_IP" POST "/v1/machines/${BOMB_ID}/exec" \
        '{"cmd":"timeout 15 sh -c \u0027while :; do :; done\u0027 >/dev/null 2>&1; exit 0","user":"root"}' >/dev/null 2>&1 &
      sample_while $!

      # Containment, asserted on the slice rather than on who did the killing.
      # A guest that OOMs its own processes and a slice that OOM-kills the VMM
      # are both containment; a slice whose memory.current sailed past
      # memory.max is not, and neither is a host that lost its free memory.
      OOM=$($SSH "root@$H_IP" "awk '/^oom_kill /{print \$2}' ${BOMB_SLICE}/memory.events" 2>/dev/null | tr -d '[:space:]')
      [ "$MEM_PEAK" -le "$MEM_MAX" ] 2>/dev/null \
        && ok "the slice held: memory.current peaked at ${MEM_PEAK} of ${MEM_MAX} (oom_kill=${OOM:-0})" \
        || bad "memory.current reached ${MEM_PEAK}, past memory.max ${MEM_MAX}"

      [ -n "$PIDS_MAX" ] && [ "$PIDS_MAX" != "max" ] \
        && ok "pids.max is ${PIDS_MAX} and pids.current peaked at ${PIDS_PEAK}" \
        || bad "pids.max is '${PIDS_MAX}'; the fork bomb has no ceiling"

      AVAIL=$($SSH "root@$H_IP" "awk '/MemAvailable/{print int(\$2/1024)}' /proc/meminfo" 2>/dev/null | tr -d '[:space:]')
      [ "${AVAIL:-0}" -gt 512 ] 2>/dev/null \
        && ok "the host still has ${AVAIL} MiB available" \
        || bad "the host is down to ${AVAIL} MiB available; the bomb reached it"
    fi

    # The neighbour, re-measured the same way it was measured before.
    AFTER_SAMPLES=$(for _ in $(seq 5); do exec_ms "$H_IP" "$NB_ID"; done | sort -n)
    AFTER_MS=$(echo "$AFTER_SAMPLES" | sed -n '3p')
    LIMIT_MS=$(( BASE_MS * 3 ))
    [ "${AFTER_MS:-0}" -gt 0 ] && [ "$AFTER_MS" -le "$LIMIT_MS" ] 2>/dev/null \
      && ok "the neighbour is at ${AFTER_MS}ms, within 3x its ${BASE_MS}ms baseline" \
      || bad "the neighbour went to ${AFTER_MS}ms against a ${LIMIT_MS}ms ceiling (3x ${BASE_MS}ms)"

    api "$H_IP" GET /v1/machines >/dev/null 2>&1 \
      && ok "hostd kept serving throughout" || bad "hostd stopped answering during the bombs"

    api "$H_IP" DELETE "/v1/machines/${BOMB_ID}" >/dev/null 2>&1
    api "$H_IP" DELETE "/v1/machines/${NB_ID}" >/dev/null 2>&1
  fi
fi

say "17. A thousand Firecracker API operations, with a bounded fd count"
# The Firecracker API accepts roughly ten connections in its whole life
# (ARCHITECTURE.md:526), which is why every call sets DisableKeepAlives. Each
# suspend, wake and checkpoint is several calls, so a thousand operations is
# two orders of magnitude past the point where a reused connection stops the
# host dead. Reverting that one line fails this loop at about operation ten.
H5_OPS=${GATE_H5_OPS:-1000}
if [ -n "$H_IP" ]; then
  H5=$(api "$H_IP" POST /v1/machines '{"vcpus":1,"mem_mib":512,"knobs":{"auto_stop":"off"}}')
  H5_ID=$(echo "$H5" | jf id)
  [ -n "$H5_ID" ] && ok "created ${H5_ID}" || bad "create failed: $H5"

  if [ -n "$H5_ID" ]; then
    # A first suspend/wake so there is something to resume from, the way
    # sections 4 and 13 do.
    api "$H_IP" POST "/v1/machines/${H5_ID}/suspend" >/dev/null 2>&1 \
      && ok "suspended once, so the machine has a snapshot" || bad "the first suspend failed"
    api "$H_IP" POST "/v1/machines/${H5_ID}/wake" >/dev/null 2>&1 \
      && ok "and woke" || bad "the first wake failed"

    # The pid is read into a variable rather than interpolated inline: an empty
    # one makes the remote command 'ls /proc//fd', which the kernel resolves to
    # the non-existent /proc/fd, so wc -l prints 0 -- and 0 -> 0 passes the fd
    # assertion below on a machine that has no Firecracker at all.
    H5_FCPID=$(fc_pid "$H_IP" "$H5_ID")
    [ -n "$H5_FCPID" ] && ok "its Firecracker is pid ${H5_FCPID}" \
      || bad "could not find the Firecracker process for ${H5_ID}; the fd counts below mean nothing"
    FD_BEFORE=$($SSH "root@$H_IP" "ls /proc/${H5_FCPID}/fd 2>/dev/null | wc -l" 2>/dev/null | tr -d '[:space:]')
    HOSTD_FD_BEFORE=$($SSH "root@$H_IP" "ls /proc/\$(pgrep -x hostd | head -1)/fd 2>/dev/null | wc -l" 2>/dev/null | tr -d '[:space:]')
    ok "Firecracker holds ${FD_BEFORE} fds and hostd holds ${HOSTD_FD_BEFORE} before the loop"

    FAILED_AT=0
    for i in $(seq "$H5_OPS"); do
      api "$H_IP" POST "/v1/machines/${H5_ID}/suspend" >/dev/null 2>&1 || { FAILED_AT=$i; break; }
      api "$H_IP" POST "/v1/machines/${H5_ID}/wake" >/dev/null 2>&1 || { FAILED_AT=$i; break; }
      if [ $((i % 10)) = 0 ]; then
        api "$H_IP" POST "/v1/machines/${H5_ID}/checkpoints" '{"comment":"h5"}' >/dev/null 2>&1 \
          || { FAILED_AT=$i; break; }
      fi
    done
    [ "$FAILED_AT" = 0 ] \
      && ok "${H5_OPS} sequential Firecracker API operations all succeeded" \
      || bad "operation ${FAILED_AT} of ${H5_OPS} failed; the API stopped answering"

    # Re-read: every wake restores into a NEW Firecracker, so the pid the loop
    # ended on is not the one it started with.
    H5_FCPID_AFTER=$(fc_pid "$H_IP" "$H5_ID")
    [ -n "$H5_FCPID_AFTER" ] || bad "no Firecracker is running for ${H5_ID} after the loop"
    FD_AFTER=$($SSH "root@$H_IP" "ls /proc/${H5_FCPID_AFTER}/fd 2>/dev/null | wc -l" 2>/dev/null | tr -d '[:space:]')
    HOSTD_FD_AFTER=$($SSH "root@$H_IP" "ls /proc/\$(pgrep -x hostd | head -1)/fd 2>/dev/null | wc -l" 2>/dev/null | tr -d '[:space:]')
    # hostd is the side that would hold a leaked keepalive connection open, so
    # it is asserted as well as Firecracker's own count.
    [ $(( FD_AFTER - FD_BEFORE )) -lt 20 ] 2>/dev/null \
      && ok "Firecracker's fd count went ${FD_BEFORE} -> ${FD_AFTER}" \
      || bad "Firecracker's fd count went ${FD_BEFORE} -> ${FD_AFTER}; connections are being kept"
    [ $(( HOSTD_FD_AFTER - HOSTD_FD_BEFORE )) -lt 20 ] 2>/dev/null \
      && ok "hostd's fd count went ${HOSTD_FD_BEFORE} -> ${HOSTD_FD_AFTER}" \
      || bad "hostd's fd count went ${HOSTD_FD_BEFORE} -> ${HOSTD_FD_AFTER}; it is leaking sockets"

    api "$H_IP" DELETE "/v1/machines/${H5_ID}" >/dev/null 2>&1
  fi
fi

say "18. hostd killed at ten random points converges to the running set"
# A single host-agent restart while VMs ran once left zombies that even SIGKILL
# could not reap. Three mechanisms answer that now and this kills across all
# three: KillMode=process, so a hostd death never signals its Firecrackers;
# Reconcile on start, which re-adopts by /proc/<pid>/comm rather than by pid
# alone, so a recycled pid is not adopted; and the reaper, which kills
# Firecrackers with no machine row once they are older than its age guard.
#
# The kill lands at a random point in the create on purpose: the interesting
# window is between spawning a Firecracker and recording it.
if [ -n "$H_IP" ]; then
  KILL_BASE=$(host_counts "$H_IP")
  KILL_IDS_BEFORE=$(machine_ids_on "$H_IP")
  KILL_SINCE=$($SSH "root@$H_IP" "date '+%Y-%m-%d %H:%M:%S'" 2>/dev/null | tr -d '\n')
  for i in $(seq 10); do
    api "$H_IP" POST /v1/machines '{"vcpus":1,"mem_mib":512}' >/dev/null 2>&1 &
    sleep "0.$((RANDOM % 9))"
    $SSH "root@$H_IP" "kill -9 \$(pgrep -x hostd) 2>/dev/null" >/dev/null 2>&1
    wait $! 2>/dev/null
    if wait_serving "$H_IP" 120; then
      ok "iteration ${i}: hostd was killed mid-create and came back"
    else
      bad "iteration ${i}: hostd never came back after the kill"
      break
    fi
  done

  # A kill that lands 0-0.8s in does not always beat the create: a create is a
  # restore, so some of the ten finished and wrote their row first. Those are
  # real machines -- the reaper only sweeps Firecrackers with NO row -- so they
  # hold a namespace, a veth half and an NBD device forever, and the baseline
  # below is unreachable until they are destroyed. This is cleanup, not an
  # assertion: what the section is about is the creates that did NOT finish.
  SURVIVORS=$(comm -13 <(echo "$KILL_IDS_BEFORE") <(machine_ids_on "$H_IP"))
  for mid in $SURVIVORS; do
    api "$H_IP" DELETE "/v1/machines/${mid}" >/dev/null 2>&1
  done
  echo "  destroyed $(echo "$SURVIVORS" | grep -c . ) machine(s) whose create outran its kill"

  $SSH "root@$H_IP" "journalctl -u hostd --since '${KILL_SINCE}' --no-pager 2>/dev/null | grep -q 're-adopted machines from a previous run'" >/dev/null 2>&1 \
    && ok "the journal shows machines re-adopted by comm across a restart" \
    || bad "no restart re-adopted anything; the running machines were abandoned"

  # Past the reaper's 5-minute interval, so an orphan has actually been swept
  # rather than merely not looked at yet.
  echo "  waiting out the reaper interval (5m) before asserting convergence"
  sleep 330

  ROWS=$(live_machines_on "$H_IP")
  FCS=$($SSH "root@$H_IP" "pgrep -xc firecracker" 2>/dev/null | tr -d '[:space:]')
  [ "${ROWS:--1}" = "${FCS:--2}" ] \
    && ok "the host runs exactly ${FCS} Firecrackers for ${ROWS} machine rows" \
    || bad "${FCS} Firecracker(s) running against ${ROWS} machine row(s); the sets diverged"

  # Everything the abandoned creates could have leaked, against the same
  # baseline the churn section uses.
  KILL_AFTER=$(host_counts "$H_IP")
  read -r KN KV KD <<< "$KILL_AFTER"
  read -r BN BV BD <<< "$KILL_BASE"
  [ "$KN" = "$BN" ] && ok "namespaces are back to ${BN}" || bad "namespaces went ${BN} -> ${KN}"
  [ "$KV" = "$BV" ] && ok "veth halves are back to ${BV}" || bad "veth halves went ${BV} -> ${KV}"
  [ "$KD" = "$BD" ] && ok "attached NBD devices are back to ${BD}" || bad "attached NBD devices went ${BD} -> ${KD}"
fi

say "19. The wedge itself, reproduced on purpose, then rebooted away"
# Everything above asserts that the disconnect ioctl runs before the kill. That
# assertion is only worth something if this run can also show what happens
# without it -- otherwise a refactor that stopped tearing anything down at all
# would leave section 14 green.
#
# So the ordering is removed, on one host, behind two environment flags that
# must BOTH be set (internal/nbd/faults.go), the wedge is reproduced, the flags
# are removed again, and the host is rebooted. Nothing else runs on this host
# afterwards: a wedged NBD device does not come back without a reboot, which is
# the whole point of the assertion.
# The LAST live host, so that on a fleet with more than one alive it is not
# the host every section above has been working on.
FAULT_IP=""
[ "${#HOSTILE_IPS[@]}" -gt 0 ] && FAULT_IP="${HOSTILE_IPS[$(( ${#HOSTILE_IPS[@]} - 1 ))]}"
if [ -z "$FAULT_IP" ]; then
  bad "no host is alive to reproduce the wedge on"
else
  $SSH "root@$FAULT_IP" "mkdir -p /etc/pilots && printf 'PILOT_FAULTS=1\nPILOT_FAULT_NBD_SKIP_DISCONNECT=1\n' >> /etc/pilots/hostd.env && systemctl restart hostd" >/dev/null 2>&1
  if wait_serving "$FAULT_IP" 120; then
    ok "armed the NBD fault on ${FAULT_IP} and hostd came back"
  else
    bad "hostd did not come back after arming the fault on ${FAULT_IP}"
  fi

  # Snapshotted BEFORE the create, so the device this machine takes can be
  # named rather than inferred from an intersection with everything else the
  # host has attached.
  WEDGE_NBD_BEFORE=$(nbd_attached "$FAULT_IP")
  WEDGE=$(api "$FAULT_IP" POST /v1/machines '{"vcpus":1,"mem_mib":512,"knobs":{"auto_stop":"off"}}')
  WEDGE_ID=$(echo "$WEDGE" | jf id)
  if [ -z "$WEDGE_ID" ]; then
    bad "could not create the machine to wedge: $WEDGE"
  else
    WEDGE_PID=$(fc_pid "$FAULT_IP" "$WEDGE_ID")
    WEDGE_OWN=$(comm -13 <(echo "$WEDGE_NBD_BEFORE") <(nbd_attached "$FAULT_IP"))
    api "$FAULT_IP" DELETE "/v1/machines/${WEDGE_ID}" >/dev/null 2>&1

    WEDGED=no
    for _ in $(seq 25); do
      STAT=$($SSH "root@$FAULT_IP" "ps -o stat= -p ${WEDGE_PID} 2>/dev/null" 2>/dev/null | tr -d '[:space:]')
      case "$STAT" in *D*) WEDGED=yes; break ;; esac
      sleep 0.2
    done
    [ "$WEDGED" = yes ] \
      && ok "without the disconnect the Firecracker wedges in D-state, so section 14 has teeth" \
      || bad "the fault did not reproduce the wedge; section 14's ordering assertion proves nothing"

    # The device THIS machine held, not the intersection of every attached
    # device with every previously attached one: other machines are running on
    # this host, so an intersection is non-empty whether the fault took effect
    # or not, and section 14's own check is the diff (comm -23), not the
    # intersection. WEDGE_OWN is what appeared when the machine was created.
    STILL=$(nbd_attached "$FAULT_IP" | comm -12 - <(echo "$WEDGE_OWN"))
    if [ -z "$WEDGE_OWN" ]; then
      bad "the wedge machine attached no NBD device, so nothing can be asserted about it"
    elif [ -n "$STILL" ]; then
      ok "and $(echo "$STILL" | tr '\n' ' ')stayed attached to a server that is gone"
    else
      bad "${WEDGE_OWN} detached anyway; the fault did not take effect"
    fi
  fi

  # Disarm before the reboot, so a host that comes back is a normal host.
  $SSH "root@$FAULT_IP" "sed -i '/^PILOT_FAULTS=/d;/^PILOT_FAULT_NBD_SKIP_DISCONNECT=/d' /etc/pilots/hostd.env" >/dev/null 2>&1
  LEFT=$($SSH "root@$FAULT_IP" "grep -c PILOT_FAULT /etc/pilots/hostd.env" 2>/dev/null | tr -d '[:space:]')
  [ "${LEFT:-1}" = "0" ] && ok "the fault flags are out of hostd.env" \
    || bad "a PILOT_FAULT line is still in /etc/pilots/hostd.env on ${FAULT_IP}"

  # A hard reset, not a graceful reboot: a process in uninterruptible sleep is
  # exactly what a graceful shutdown waits forever for.
  FDOM=$(sudo virsh list --name 2>/dev/null | while read -r d; do
    [ -n "$d" ] && sudo virsh domifaddr "$d" --source lease 2>/dev/null | grep -q "$FAULT_IP" && echo "$d"
  done | head -1)
  if [ -n "$FDOM" ]; then
    sudo virsh reset "$FDOM" >/dev/null 2>&1 && ok "hard reset ${FDOM}" || bad "could not reset ${FDOM}"
  else
    $SSH "root@$FAULT_IP" "echo b > /proc/sysrq-trigger" >/dev/null 2>&1
    ok "asked ${FAULT_IP} to reset through sysrq (no libvirt domain matched it)"
  fi

  if wait_serving "$FAULT_IP" 180; then
    ok "${FAULT_IP} serves again after the reboot"
  else
    bad "${FAULT_IP} did not come back within 180s; the rig is one host down"
  fi
fi

say "Result"
echo "  ${PASS} passed, ${FAIL} failed"
echo
echo "This run left the rig changed on purpose:"
echo "  - three hosts are powered off (steps 8, 12 and 13); sudo virsh start <domain>"
echo "  - the fleet is one host bigger (step 11), and cluster.env records it,"
echo "    so the next run adds another. cluster-down.sh resets it."
echo "  - one host was hard reset (step 19) after the NBD wedge was reproduced"
echo "    on it on purpose; the fault flags were removed from its hostd.env first."
[ "$FAIL" = 0 ] || exit 1
