#!/usr/bin/env bash
# Start the object store a single-box pilots needs: a throwaway MinIO.
#
#   sudo scripts/local-s3.sh
#
# Runs in the FOREGROUND. Stop it with Ctrl-C.
#
# Object storage is the only truth for machine state (AGENTS.md hard rule 3),
# so this is the first thing that has to exist: with no bucket the uploader is
# fc.UnconfiguredStore{} and the very first POST /v1/machines fails while
# chunkifying the golden rootfs. Nothing downstream can be tried until this
# runs.
#
# The credentials and the bucket name are NOT invented here. They are the ones
# scripts/cluster/cluster-bootstrap.sh already hardcodes, whose comment says
# "the credentials below are fixed on purpose so the rig needs no setup" -- a
# sentence that was false because nothing in the repo started a MinIO. This is
# what makes it true, and it is why the listener is 0.0.0.0 rather than
# loopback: the three-node rig reaches this same MinIO at ${NET_SUBNET}.1:9000
# over the libvirt bridge, so one store serves both the laptop-native host and
# the VMs.
#
# These credentials are worthless outside a developer's own machine. A real
# host gets every one of them from the operator, through host-bootstrap.sh,
# which this script does not touch.
set -euo pipefail

# Pinned and checksummed for the same reason fetch-firecracker.sh pins
# Firecracker: an unpinned binary is a different program on every laptop, and
# the one bug this arrangement can produce -- a store that answers differently
# here than on the rig -- is the expensive kind to chase.
MINIO_VERSION="RELEASE.2025-09-07T16-13-09Z"
MINIO_SHA256=7c5bd8512c6e966455b1d198209358b2d191c77a83ab377c4073281065fb855f

PREFIX="${PREFIX:-/opt/pilots}"
BIN="$PREFIX/bin/minio"
DATA="${PILOT_S3_DATA:-/var/lib/pilots/minio}"
ADDR="${PILOT_S3_ADDR:-0.0.0.0:9000}"

# Fixed on purpose; see the header. cluster-bootstrap.sh:35-43 is the other
# half of this contract.
BUCKET="${PILOT_S3_BUCKET:-pilots}"
ACCESS_KEY="${PILOT_S3_ACCESS_KEY:-pilots}"
SECRET_KEY="${PILOT_S3_SECRET_KEY:-pilots-secret}"

[ "$(id -u)" = 0 ] || {
  echo "local-s3.sh must run as root: it installs into $PREFIX/bin and keeps" >&2
  echo "its data under $DATA, both of which are root-owned on a pilots host." >&2
  echo "  sudo scripts/local-s3.sh" >&2
  exit 1
}

arch="$(uname -m)"
[ "$arch" = "x86_64" ] || { echo "unsupported arch: $arch" >&2; exit 1; }

# A clearer refusal than MinIO's own, which says "Specified port is already in
# use" and names neither the port nor what holds it. A hand-started store from
# before this script existed is exactly what is likely to be sitting there.
PORT="${ADDR##*:}"
if command -v ss >/dev/null 2>&1 && ss -lnt "sport = :$PORT" 2>/dev/null | grep -q LISTEN; then
  echo "something already listens on port $PORT:" >&2
  ss -lntp "sport = :$PORT" >&2 || true
  echo "stop it, or re-run with PILOT_S3_ADDR=0.0.0.0:<other-port>" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# The binary. MinIO's server is not packaged by Arch (only the client, as
# minio-client, whose binary is named mcli there), so it is fetched the way
# fetch-firecracker.sh fetches Firecracker: pinned URL, published checksum,
# and a presence check so a re-run is a no-op.
if [ -x "$BIN" ] && "$BIN" --version 2>/dev/null | grep -q "$MINIO_VERSION"; then
  echo "minio $MINIO_VERSION already installed at $BIN"
else
  url="https://dl.min.io/server/minio/release/linux-amd64/archive/minio.${MINIO_VERSION}"
  tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT

  echo "==> downloading $url"
  curl -fsSL "$url" -o "$tmp/minio"

  echo "==> verifying sha256"
  echo "${MINIO_SHA256}  $tmp/minio" | sha256sum -c - || {
    echo "CHECKSUM MISMATCH -- refusing to install" >&2; exit 1; }

  install -d -m0755 "$PREFIX/bin"
  install -m0755 "$tmp/minio" "$BIN"
  echo "==> installed $("$BIN" --version | head -1)"
fi

# ---------------------------------------------------------------------------
# The bucket. A single-drive MinIO reads each top-level directory under its
# data root as a bucket, so creating it here means the bucket exists before
# the first request rather than after a client happens to run -- and mkdir -p
# is the whole of "idempotent".
install -d -m0755 "$DATA" "$DATA/$BUCKET"

echo "==> starting minio on $ADDR (bucket: $BUCKET, data: $DATA)"
MINIO_ROOT_USER="$ACCESS_KEY" MINIO_ROOT_PASSWORD="$SECRET_KEY" \
  "$BIN" server --address "$ADDR" --console-address 127.0.0.1:9001 "$DATA" &
minio_pid=$!
trap 'kill "$minio_pid" 2>/dev/null || true' EXIT INT TERM

health="http://127.0.0.1:${PORT}/minio/health/live"
for _ in $(seq 1 60); do
  curl -fsS -o /dev/null "$health" 2>/dev/null && break
  kill -0 "$minio_pid" 2>/dev/null || { echo "minio exited during startup" >&2; exit 1; }
  sleep 0.5
done
curl -fsS -o /dev/null "$health" 2>/dev/null || {
  echo "minio did not become healthy at $health" >&2; exit 1; }

# Confirm through the S3 API rather than trusting the directory, when a client
# is on PATH. Arch's package installs it as mcli; upstream's is mc.
for client in mc mcli; do
  command -v "$client" >/dev/null 2>&1 || continue
  "$client" alias set pilots-local "http://127.0.0.1:${PORT}" \
    "$ACCESS_KEY" "$SECRET_KEY" >/dev/null
  "$client" mb --ignore-existing "pilots-local/$BUCKET" >/dev/null
  echo "==> bucket confirmed over the S3 API with $client"
  break
done

cat <<SUMMARY

object store is up

  PILOT_S3_ENDPOINT=http://127.0.0.1:${PORT}     (this machine)
  PILOT_S3_ENDPOINT=http://<bridge-ip>:${PORT}   (the three-node rig)
  PILOT_S3_BUCKET=$BUCKET
  PILOT_S3_ACCESS_KEY=$ACCESS_KEY
  PILOT_S3_SECRET_KEY=$SECRET_KEY

leave this running; start hostd with scripts/local-host.sh in another shell
SUMMARY

wait "$minio_pid"
