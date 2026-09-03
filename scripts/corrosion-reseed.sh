#!/usr/bin/env bash
# Re-seed one host's corrosion replica from its peers.
#
#   scripts/corrosion-reseed.sh <ip> [--from <survivor-ip>] [--dry-run]
#
# WHEN TO RUN IT. A host whose local replica is corrupt, wedged, or so far
# behind that anti-entropy is not catching up. The symptom is a host whose
# /v1/hosts or /v1/machines disagrees with every peer and stays disagreeing.
# It is also rehearsed deliberately, because a recovery path nobody has run is
# not a recovery path.
#
# WHAT IT STOPS, AND WHY THAT ORDER MATTERS MOST. hostd is stopped FIRST.
# hostd's reaper kills any Firecracker it has no row for after 60s, so hostd
# running against a freshly emptied replica would destroy every machine on the
# host -- the exact machines the re-seed exists to save. hostd's unit uses
# KillMode=process, so stopping the daemon leaves every machine running and
# serving; it re-adopts them on the way back up.
#
# WHERE THE OLD STORE GOES. Moved aside into a timestamped directory, never
# deleted. A replica that went wrong is the evidence for the incident entry
# this run owes (docs/incidents/), and it is unrecoverable once removed.
#
# AFTERWARDS. Write it up. Every re-seed gets a docs/incidents/ entry, this
# one included.
set -euo pipefail

IP="${1:-}"
FROM=""
DRY_RUN=0
SSH_OPTS="${SSH_OPTS:--o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null}"
CONVERGE_DEADLINE="${CONVERGE_DEADLINE:-60}"
TABLES="hosts machines volumes services releases"

shift || true
while [ $# -gt 0 ]; do
  case "$1" in
    --from) FROM="${2:-}"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

[ -n "$IP" ] || {
  echo "usage: $0 <ip> [--from <survivor-ip>] [--dry-run]" >&2
  exit 2
}

say() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
on_host() {
  if [ "$DRY_RUN" = 1 ]; then
    printf '  would run on %s: %s\n' "$IP" "$*"
    return 0
  fi
  ssh $SSH_OPTS "root@${IP}" "$@"
}
on_peer() {
  local host="$1"; shift
  ssh $SSH_OPTS "root@${host}" "$@"
}

# Ask a host's corrosion for a single scalar. The token is read from the host's
# own config rather than passed in: the operator running a recovery should not
# have to hold the cluster secret to run it.
corro_count() {
  local host="$1" table="$2"
  on_peer "$host" "TOKEN=\$(grep '^PILOT_CORROSION_TOKEN=' /etc/pilots/config | cut -d= -f2-); \
    curl -sf --http2-prior-knowledge http://127.0.0.1:51002/v1/queries \
      -H 'Content-Type: application/json' \
      -H \"Authorization: Bearer \$TOKEN\" \
      -d '\"SELECT count(*) FROM ${table}\"'" |
    python3 -c 'import sys,json
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    obj = json.loads(line)
    row = obj.get("row")
    if row:
        print(row[1][0]); break' 2>/dev/null || echo ""
}

counts_of() {
  local host="$1" out=""
  for t in $TABLES; do
    out="${out}${t}=$(corro_count "$host" "$t") "
  done
  printf '%s' "${out% }"
}

# Whether every table in a counts_of line reported an actual number. A query
# that failed leaves "machines=" behind, which is still a non-empty line.
all_counted() {
  local pair
  for pair in $1; do
    case "${pair#*=}" in
      ''|*[!0-9]*) return 1 ;;
    esac
  done
  [ -n "$1" ]
}

live_hosts() {
  curl -sf -m 10 "http://${1}:8080/v1/hosts" 2>/dev/null |
    python3 -c 'import sys,json;print(len(json.load(sys.stdin) or []))' 2>/dev/null || echo 0
}

say "Re-seeding ${IP}${FROM:+ from ${FROM}}"

# ---------------------------------------------------------------------------
# 1. Refuse the runs that cannot work.
#
# A host is re-seeded FROM its peers. With no peers there is nothing to re-seed
# from, and wiping the store would destroy the only copy of this host's own
# rows. And a survivor that cannot see the target is not a survivor for this
# purpose: the two are partitioned, and the re-seed would converge on an empty
# view of a fleet that is fine.
if [ "$FROM" = "$IP" ]; then
  echo "  --from cannot be the host being re-seeded: a wiped replica cannot" >&2
  echo "  re-seed from itself. Name a peer that still has the state." >&2
  exit 2
fi

if [ -z "$FROM" ]; then
  PEERS=$(live_hosts "$IP")
  if [ "$PEERS" -lt 2 ]; then
    echo "  ${IP} sees ${PEERS} host(s). With no peer there is nothing to" >&2
    echo "  re-seed from, and wiping the store would destroy the only copy" >&2
    echo "  of this host's own rows. Pass --from <survivor-ip>." >&2
    exit 1
  fi
  echo "  ${IP} sees ${PEERS} hosts, but no survivor was named."
  echo "  Pass --from <survivor-ip>: convergence is measured against a peer."
  exit 2
fi

SEEN=$(curl -sf -m 10 "http://${FROM}:8080/v1/hosts" 2>/dev/null |
  python3 -c "import sys,json;print(sum(1 for h in (json.load(sys.stdin) or []) if h.get('public_ip')=='${IP}'))" 2>/dev/null || echo 0)
if [ "$SEEN" = 0 ]; then
  echo "  ${FROM} does not list ${IP} in /v1/hosts. A host nobody else can" >&2
  echo "  see has nothing to re-seed from -- fix the mesh first, or the" >&2
  echo "  re-seed converges on a view that does not include this host." >&2
  exit 1
fi
echo "  ${FROM} can see ${IP}"

BEFORE=$(counts_of "$FROM")
echo "  survivor counts: ${BEFORE}"

# ---------------------------------------------------------------------------
# 2. hostd FIRST. See the header: the reaper against an empty replica destroys
#    the machines this run exists to save. KillMode=process means the machines
#    keep serving with the daemon down.
say "Stopping hostd on ${IP} (machines keep serving)"
on_host "systemctl stop hostd"

# 3. Then corrosion, so the store is not being written while it is moved.
say "Stopping corrosion"
on_host "systemctl stop corrosion"

# 4. Move the store aside. Never rm: this file is the evidence.
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
BACKUP="/var/lib/pilots/corrosion/reseed-${STAMP}"
say "Moving the old store to ${BACKUP}"
on_host "mkdir -p '${BACKUP}' && \
  mv /var/lib/pilots/corrosion/store.db* '${BACKUP}/' 2>/dev/null || true; \
  if [ -d /var/lib/pilots/corrosion/subscriptions ]; then \
    mv /var/lib/pilots/corrosion/subscriptions '${BACKUP}/'; \
  fi"

# 5. Start corrosion and wait for it to apply the schema. It answers queries
#    only after that, so a successful query IS the readiness signal.
say "Starting corrosion and waiting for the schema"
on_host "systemctl start corrosion; \
  for i in \$(seq 120); do \
    TOKEN=\$(grep '^PILOT_CORROSION_TOKEN=' /etc/pilots/config | cut -d= -f2-); \
    if curl -sf --http2-prior-knowledge http://127.0.0.1:51002/v1/queries \
         -H 'Content-Type: application/json' \
         -H \"Authorization: Bearer \$TOKEN\" \
         -d '\"SELECT count(*) FROM hosts\"' >/dev/null 2>&1; then exit 0; fi; \
    sleep 1; \
  done; \
  echo '  corrosion did not apply its schema within 120s' >&2; exit 1"

# ---------------------------------------------------------------------------
# 6. Convergence. Three consecutive agreeing polls, not one: a single match
#    during a sync in progress is a coincidence, and declaring victory on it is
#    how a half-replicated replica gets handed back to a hostd that will act on
#    it.
say "Waiting for convergence (deadline ${CONVERGE_DEADLINE}s)"
if [ "$DRY_RUN" = 1 ]; then
  echo "  would poll ${TABLES// /, } on ${IP} against ${FROM}"
else
  AGREED=0
  CONVERGED=0
  TARGET_COUNTS=""
  SURVIVOR_COUNTS=""
  DEADLINE=$(( $(date +%s) + CONVERGE_DEADLINE ))
  while [ "$(date +%s)" -lt "$DEADLINE" ]; do
    TARGET_COUNTS=$(counts_of "$IP")
    SURVIVOR_COUNTS=$(counts_of "$FROM")
    # Every table must have reported a NUMBER. counts_of always returns a
    # non-empty string -- a failed query leaves "hosts= machines= ..." -- so a
    # plain -n test passes when every count failed the same way on both hosts,
    # and two identically broken measurements would read as convergence. That
    # hands an unverified replica back to hostd, whose reaper then destroys
    # the machines this run exists to save.
    if all_counted "$TARGET_COUNTS" && all_counted "$SURVIVOR_COUNTS" &&
       [ "$TARGET_COUNTS" = "$SURVIVOR_COUNTS" ]; then
      AGREED=$((AGREED + 1))
      if [ "$AGREED" -ge 3 ]; then CONVERGED=1; break; fi
    else
      AGREED=0
    fi
    sleep 2
  done
  if [ "$CONVERGED" != 1 ]; then
    echo "  did not converge within ${CONVERGE_DEADLINE}s." >&2
    echo "    ${IP}:   ${TARGET_COUNTS}" >&2
    echo "    ${FROM}: ${SURVIVOR_COUNTS}" >&2
    echo "  hostd is still stopped on ${IP}: its machines are serving and will" >&2
    echo "  NOT be reaped. Investigate before starting it." >&2
    exit 1
  fi
  echo "  converged: ${TARGET_COUNTS}"
fi

# 7. hostd back, and it re-adopts the machines that never stopped.
say "Starting hostd"
on_host "systemctl start hostd; \
  for i in \$(seq 180); do \
    curl -sf http://127.0.0.1:8080/v1/health >/dev/null 2>&1 && exit 0; \
    sleep 1; \
  done; \
  echo '  hostd did not start serving within 180s' >&2; exit 1"

say "Re-seeded ${IP}"
echo "  old store: ${BACKUP} on ${IP} (keep it: it is the evidence)"
echo "  verify:    scripts/cluster/gate.sh section 19, or /v1/hosts on every host"
echo "  write it up in docs/incidents/ -- every re-seed gets an entry."
