#!/usr/bin/env bash
# Start the object store a single box needs, in the foreground.
#
# This is the FIRST thing that has to exist, because S3 is the only truth for
# machine state (AGENTS.md hard rule 3) and the very first POST /v1/machines
# proves it: createFromTemplate calls EnsureTemplate, which falls through to
# buildTemplate, which calls fc.UploadBuild. With no bucket configured hostd
# builds an fc.UnconfiguredStore whose every method returns "fc: no object
# storage is configured", so the first create fails inside the template build
# and nothing downstream can be tried at all.
#
# The credentials and the bucket below are NOT invented here. They are the ones
# scripts/cluster/cluster-bootstrap.sh already hardcodes, whose comment says
# "The credentials below are fixed on purpose so the rig needs no setup" --
# except that nothing in this repo ever started the store that sentence assumes.
# This script is what makes it true. It listens on 0.0.0.0 so one store serves
# both the laptop-native host at 127.0.0.1:9000 and the three-node rig at
# ${NET_SUBNET}.1:9000 over the libvirt bridge.
#
# These credentials are worthless outside a developer's own machine. A real
# host gets every S3 value from the operator through scripts/host-bootstrap.sh,
# which this script does not touch and does not read.
#
# On the machine this was written for the job was being done by a hand-started
# Docker container named pilots-minio, bind-mounting /var/lib/pilots-minio.
# Stop it before running this (a 0.0.0.0 bind loses to a bind on any address of
# the same port), and pass PILOT_S3_DATA=/var/lib/pilots-minio to keep serving
# the objects it accumulated.
set -euo pipefail

# Pinned for the reason scripts/fetch-firecracker.sh pins Firecracker: an
# unpinned binary is a different program on every laptop, and a store that
# answers differently here than on the rig is the expensive kind of bug. The
# checksum is the one upstream serves at
# https://dl.min.io/server/minio/release/linux-amd64/minio.sha256sum
MINIO_VERSION="RELEASE.2025-09-07T16-13-09Z"
MINIO_SHA256=7c5bd8512c6e966455b1d198209358b2d191c77a83ab377c4073281065fb855f

PREFIX="${PREFIX:-/opt/pilots}"
BIN="$PREFIX/bin/minio"
# A sibling of the directories hostd owns, and deliberately NOT one that
# scripts/e2e-restart.sh wipes -- that script clears machine state, not objects.
DATA="${PILOT_S3_DATA:-/var/lib/pilots/minio}"
ADDR="${PILOT_S3_ADDR:-0.0.0.0:9000}"
BUCKET="${PILOT_S3_BUCKET:-pilots}"
ACCESS_KEY="${PILOT_S3_ACCESS_KEY:-pilots}"
SECRET_KEY="${PILOT_S3_SECRET_KEY:-pilots-secret}"
PORT="${ADDR##*:}"

[ "$(id -u)" = 0 ] || {
  echo "local-s3.sh installs into $PREFIX/bin and keeps data under a root-owned tree." >&2
  echo "  sudo scripts/local-s3.sh" >&2
  exit 1; }

arch="$(uname -m)"
[ "$arch" = "x86_64" ] || { echo "unsupported arch: $arch" >&2; exit 1; }

# MinIO's own failure here is "Specified port is already in use" and names
# nothing, so say who holds it before starting anything.
if ss -lnt "sport = :$PORT" 2>/dev/null | grep -q LISTEN; then
  echo "port $PORT is already held:" >&2
  ss -lntp "sport = :$PORT" >&2 || true
  echo "stop it, or re-run with PILOT_S3_ADDR=0.0.0.0:<other-port>" >&2
  exit 1
fi

if [ -x "$BIN" ] && "$BIN" --version 2>/dev/null | grep -q "$MINIO_VERSION"; then
  echo "==> minio $MINIO_VERSION already installed at $BIN"
else
  url="https://dl.min.io/server/minio/release/linux-amd64/archive/minio.${MINIO_VERSION}"
  tmp="$(mktemp -d)"
  echo "==> downloading $url"
  curl -fsSL "$url" -o "$tmp/minio"

  echo "==> verifying sha256"
  echo "${MINIO_SHA256}  $tmp/minio" | sha256sum -c - || {
    rm -rf "$tmp"
    echo "CHECKSUM MISMATCH -- refusing to install" >&2; exit 1; }

  install -d -m0755 "$PREFIX/bin"
  install -m0755 "$tmp/minio" "$BIN"
  # Removed here and not in an EXIT trap: the download is ~110 MB and the trap
  # set below for the server process would replace an earlier EXIT handler.
  rm -rf "$tmp"
  echo "==> installed $BIN"
fi

# A single-drive MinIO reads each top-level directory under its data root as a
# bucket, so creating it here is the whole of "the bucket exists before the
# first request" -- and install -d is the whole of "idempotent".
install -d -m0755 "$DATA" "$DATA/$BUCKET"

echo "==> starting minio on $ADDR (data $DATA)"
MINIO_ROOT_USER="$ACCESS_KEY" MINIO_ROOT_PASSWORD="$SECRET_KEY" \
  "$BIN" server --address "$ADDR" --console-address 127.0.0.1:9001 "$DATA" &
minio_pid=$!
trap 'kill "$minio_pid" 2>/dev/null || true' EXIT INT TERM

for _ in $(seq 60); do
  if ! kill -0 "$minio_pid" 2>/dev/null; then
    echo "minio exited before it became healthy" >&2; exit 1
  fi
  if curl -fsS "http://127.0.0.1:${PORT}/minio/health/live" >/dev/null 2>&1; then
    healthy=1; break
  fi
  sleep 0.5
done
[ "${healthy:-}" = 1 ] || { echo "minio did not become healthy within 30s" >&2; exit 1; }

# Confirm through the S3 API when a client is on PATH. Arch packages the MinIO
# client as mcli, other distributions as mc. Neither present is fine: the
# directory created above already is the bucket.
for client in mc mcli; do
  if command -v "$client" >/dev/null 2>&1; then
    "$client" --no-color alias set pilots-local "http://127.0.0.1:${PORT}" \
      "$ACCESS_KEY" "$SECRET_KEY" >/dev/null
    "$client" --no-color mb --ignore-existing "pilots-local/$BUCKET" >/dev/null
    echo "==> bucket $BUCKET confirmed through the S3 API with $client"
    break
  fi
done

cat <<EOF

==> object store is up. The config values it serves:

    PILOT_S3_ENDPOINT=http://127.0.0.1:${PORT}      # this machine
    PILOT_S3_ENDPOINT=http://192.168.124.1:${PORT}  # the three-node rig, over the libvirt bridge
    PILOT_S3_BUCKET=${BUCKET}
    PILOT_S3_ACCESS_KEY=${ACCESS_KEY}
    PILOT_S3_SECRET_KEY=${SECRET_KEY}

Leave this running. Start hostd with 'sudo scripts/local-host.sh' in another
shell; see docs/local.md for the rest of the runbook.
EOF

wait "$minio_pid"
