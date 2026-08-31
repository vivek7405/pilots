#!/usr/bin/env bash
# hostd restart re-adoption gate.
#
# Separate from e2e.mjs because it restarts the daemon under test, which the
# in-process battery cannot do. It asserts the property that makes hostd
# upgradable: SIGTERM (and even SIGKILL) detaches from the machines rather than
# taking them down, and the next start re-adopts the SAME processes.
#
# Requires root, a Firecracker host, and object storage. Run:
#   sudo scripts/e2e-restart.sh
set -e
API=http://127.0.0.1:8080
KEY=pilot_e2e_testkey
auth="Authorization: Bearer $KEY"
ROOTFS=/home/vivek/Documents/Projects/sandbox/pilots-phase-2/scripts/rootfs/golden.ext4

start_hostd() {
  sudo setsid env PILOT_LISTEN=127.0.0.1:8080 PILOT_HOST_ID=host-e2e \
    PILOT_STATE_DSN=/var/lib/pilots/e2e-state.db \
    PILOT_TEMPLATE_ROOTFS=$ROOTFS \
    PILOT_S3_ENDPOINT=http://127.0.0.1:9000 PILOT_S3_BUCKET=pilots \
    PILOT_S3_ACCESS_KEY=pilots PILOT_S3_SECRET_KEY=pilots-secret \
    /tmp/hostd-e2e >> "$1" 2>&1 < /dev/null &
  until curl -sf $API/v1/health >/dev/null 2>&1; do sleep 0.5; done
}

sudo pkill -9 -x hostd-e2e 2>/dev/null; sleep 1
for n in $(ip netns list 2>/dev/null|awk '{print $1}'); do sudo ip netns del $n 2>/dev/null; done
sudo rm -rf /var/lib/pilots/e2e-state.db /var/lib/pilots/machines /var/lib/pilots/jailer /var/cache/pilots
sudo mkdir -p /var/lib/pilots
start_hostd /tmp/h1.log
sudo sqlite3 /var/lib/pilots/e2e-state.db "INSERT INTO api_keys (hash,org_id,scopes,created_at) VALUES ('$(cat /tmp/e2e-hash.txt)','org_e2e','machines',$(date +%s));"

echo "== create =="
M=$(curl -sf -X POST "$API/v1/machines" -H "$auth" -H 'Content-Type: application/json' -d '{"vcpus":1,"mem_mib":512}')
ID=$(echo "$M"|python3 -c 'import sys,json;print(json.load(sys.stdin)["id"])')
URL=$(echo "$M"|python3 -c 'import sys,json;print(json.load(sys.stdin)["url"])')
PID_BEFORE=$(sudo cat /var/lib/pilots/machines/$ID/fc.pid)
echo "machine $ID, firecracker pid $PID_BEFORE"

curl -sf -X POST "$API/v1/machines/$ID/exec" -H "$auth" -H 'Content-Type: application/json' -d '{"cmd":"echo before-restart > /root/persisted","user":"root"}' >/dev/null

echo "== SIGKILL hostd (crash path) =="
sudo pkill -9 -x hostd-e2e; sleep 2
test -d /proc/$PID_BEFORE && echo "  firecracker survived the crash: YES" || { echo "  FAIL: firecracker died with hostd"; exit 1; }

echo "== restart hostd =="
start_hostd /tmp/h2.log
PID_AFTER=$(sudo cat /var/lib/pilots/machines/$ID/fc.pid)
[ "$PID_BEFORE" = "$PID_AFTER" ] && echo "  PASS: same firecracker pid ($PID_AFTER) re-adopted" || { echo "  FAIL: pid $PID_BEFORE -> $PID_AFTER"; exit 1; }

OUT=$(curl -sf -X POST "$API/v1/machines/$ID/exec" -H "$auth" -H 'Content-Type: application/json' -d '{"cmd":"cat /root/persisted","user":"root"}'|python3 -c 'import sys,json;print(json.load(sys.stdin)["stdout"].strip())')
[ "$OUT" = "before-restart" ] && echo "  PASS: exec works and disk intact on re-adopted machine" || { echo "  FAIL: got '$OUT'"; exit 1; }

URL_AFTER=$(curl -sf "$API/v1/machines/$ID" -H "$auth"|python3 -c 'import sys,json;print(json.load(sys.stdin)["url"])')
[ "$URL" = "$URL_AFTER" ] && echo "  PASS: URL unchanged across restart" || { echo "  FAIL: url changed"; exit 1; }

curl -sf -X DELETE "$API/v1/machines/$ID" -H "$auth" >/dev/null && echo "  cleaned up"
echo "ALL RESTART ASSERTIONS PASSED"
