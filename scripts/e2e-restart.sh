#!/usr/bin/env bash
# hostd restart re-adoption gate.
#
# Separate from e2e.mjs because it restarts the daemon under test, which the
# in-process battery cannot do. It asserts the property that makes hostd
# upgradable: SIGKILL detaches from the machines rather than taking them down,
# and the next start re-adopts the SAME processes with their disks intact and
# their URLs unchanged.
#
# Requires root, a Firecracker host, and object storage. Run:
#   sudo scripts/e2e-restart.sh
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
API="${PILOT_API:-http://127.0.0.1:8080}"
HOSTD="${HOSTD_BIN:-$REPO/apps/hostd/hostd}"
ROOTFS="${PILOT_TEMPLATE_ROOTFS:-/var/lib/pilots/templates/golden.ext4}"
STATE_DSN="${PILOT_STATE_DSN:-/var/lib/pilots/e2e-state.db}"

[ "$(id -u)" = 0 ] || { echo "must run as root" >&2; exit 1; }
for f in "$HOSTD" "$ROOTFS"; do
  [ -e "$f" ] || { echo "missing: $f (build hostd and the golden rootfs first)" >&2; exit 1; }
done

jsonfield() { python3 -c "import sys,json;print(json.load(sys.stdin)[\"$1\"])"; }

start_hostd() {
  setsid env PILOT_LISTEN="${API#http://}" PILOT_HOST_ID=host-e2e \
    PILOT_STATE_DSN="$STATE_DSN" PILOT_TEMPLATE_ROOTFS="$ROOTFS" \
    PILOT_S3_ENDPOINT="${PILOT_S3_ENDPOINT:-http://127.0.0.1:9000}" \
    PILOT_S3_BUCKET="${PILOT_S3_BUCKET:-pilots}" \
    PILOT_S3_ACCESS_KEY="${PILOT_S3_ACCESS_KEY:-pilots}" \
    PILOT_S3_SECRET_KEY="${PILOT_S3_SECRET_KEY:-pilots-secret}" \
    "$HOSTD" >> "$1" 2>&1 < /dev/null &
  until curl -sf "$API/v1/health" >/dev/null 2>&1; do sleep 0.5; done
}

# `|| true` on every teardown: a clean host has nothing to kill, and `set -e`
# would otherwise abort the gate before it asserted anything.
pkill -9 -x "$(basename "$HOSTD")" 2>/dev/null || true
sleep 1
for n in $(ip netns list 2>/dev/null | awk '{print $1}'); do ip netns del "$n" 2>/dev/null || true; done
rm -rf "$STATE_DSN" /var/lib/pilots/machines /var/lib/pilots/jailer /var/cache/pilots
mkdir -p "$(dirname "$STATE_DSN")"

start_hostd /tmp/pilots-restart-1.log

# Mint an API key: hostd authenticates against hashes in its local state.
#
# `hostd bootstrap-key` rather than a sqlite3 insert, so the key shape, the
# hash and the scopes have one implementation. It mints an admin key, which
# this script needs because tenancy scopes the routes it drives.
KEY="${PILOT_API_KEY:-$(PILOT_STATE_DSN="$STATE_DSN" "$HOSTD" bootstrap-key | tail -1)}"
auth="Authorization: Bearer $KEY"
case "$KEY" in pilot_*) ;; *) echo "bootstrap-key did not mint a key" >&2; exit 1 ;; esac

echo "== create =="
M=$(curl -sf -X POST "$API/v1/machines" -H "$auth" -H 'Content-Type: application/json' \
      -d '{"vcpus":1,"mem_mib":512}')
ID=$(echo "$M" | jsonfield id)
URL=$(echo "$M" | jsonfield url)
PID_BEFORE=$(cat "/var/lib/pilots/machines/$ID/fc.pid")
echo "machine $ID, firecracker pid $PID_BEFORE"

curl -sf -X POST "$API/v1/machines/$ID/exec" -H "$auth" -H 'Content-Type: application/json' \
  -d '{"cmd":"echo before-restart > /root/persisted","user":"root"}' >/dev/null

echo "== SIGKILL hostd (the crash path, no graceful shutdown) =="
pkill -9 -x "$(basename "$HOSTD")" || true
sleep 2
# /proc, not kill -0: the process is root-owned and kill -0 reports EPERM
# rather than absence when the caller is not root.
[ -d "/proc/$PID_BEFORE" ] && echo "  firecracker survived: YES" \
  || { echo "  FAIL: firecracker died with hostd"; exit 1; }

echo "== restart hostd =="
start_hostd /tmp/pilots-restart-2.log
PID_AFTER=$(cat "/var/lib/pilots/machines/$ID/fc.pid")
[ "$PID_BEFORE" = "$PID_AFTER" ] && echo "  PASS: same firecracker pid ($PID_AFTER) re-adopted" \
  || { echo "  FAIL: pid $PID_BEFORE -> $PID_AFTER"; exit 1; }

OUT=$(curl -sf -X POST "$API/v1/machines/$ID/exec" -H "$auth" -H 'Content-Type: application/json' \
        -d '{"cmd":"cat /root/persisted","user":"root"}' | jsonfield stdout)
[ "$(echo "$OUT" | tr -d '[:space:]')" = "before-restart" ] \
  && echo "  PASS: exec works and the disk is intact on the re-adopted machine" \
  || { echo "  FAIL: got '$OUT'"; exit 1; }

URL_AFTER=$(curl -sf "$API/v1/machines/$ID" -H "$auth" | jsonfield url)
[ "$URL" = "$URL_AFTER" ] && echo "  PASS: URL unchanged across restart" \
  || { echo "  FAIL: URL changed to $URL_AFTER"; exit 1; }

# The block and fault servers outlive hostd for the same reason firecracker
# does, so the restart has to pick THOSE back up too. It is not enough for the
# machine to be routable: a restart that adopts the VM but not its handlers
# leaves the device attached with nothing holding a handle to it, and destroy
# then leaks both processes and burns that /dev/nbdN until the host reboots.
NBD_PID=$(python3 -c "import json;print(json.load(open('/var/lib/pilots/machines/$ID/state.json')).get('nbd_pid',0))")
NBD_IDX=$(python3 -c "import json;print(json.load(open('/var/lib/pilots/machines/$ID/state.json')).get('nbd_index',-1))")
UFFD_PID=$(python3 -c "import json;print(json.load(open('/var/lib/pilots/machines/$ID/state.json')).get('uffd_pid',0))")
[ "$NBD_PID" -gt 0 ] && [ -d "/proc/$NBD_PID" ]   && echo "  PASS: the block server survived and is recorded (pid $NBD_PID, nbd$NBD_IDX)"   || { echo "  FAIL: block server pid '$NBD_PID' not recorded or not running"; exit 1; }
[ "$UFFD_PID" -gt 0 ] && [ -d "/proc/$UFFD_PID" ]   && echo "  PASS: the fault server survived and is recorded (pid $UFFD_PID)"   || { echo "  FAIL: fault server pid '$UFFD_PID' not recorded or not running"; exit 1; }

curl -sf -X DELETE "$API/v1/machines/$ID" -H "$auth" >/dev/null && echo "  cleaned up"
sleep 1

# Destroy on a RE-ADOPTED machine has to reach the handlers it never spawned.
[ -d "/proc/$NBD_PID" ] && { echo "  FAIL: the block server outlived destroy"; exit 1; }   || echo "  PASS: the block server was stopped by destroy"
[ -d "/proc/$UFFD_PID" ] && { echo "  FAIL: the fault server outlived destroy"; exit 1; }   || echo "  PASS: the fault server was stopped by destroy"
[ "$(cat "/sys/block/nbd$NBD_IDX/pid" 2>/dev/null)" = "" ]   && echo "  PASS: /dev/nbd$NBD_IDX was released back to the kernel"   || { echo "  FAIL: /dev/nbd$NBD_IDX is still attached"; exit 1; }

pkill -9 -x "$(basename "$HOSTD")" 2>/dev/null || true
echo "ALL RESTART ASSERTIONS PASSED"
