#!/bin/sh
# Install the `pilot` CLI.
#
#   curl -fsSL https://raw.githubusercontent.com/vivek7405/pilots/main/scripts/install-pilot.sh | sh
#
# POSIX sh, not bash: this is piped into whatever /bin/sh is on the machine,
# and a bashism here fails on Alpine, on Debian's dash, and inside a slim
# container image, which are three of the places somebody most wants one
# static binary.
#
# It downloads the asset named `pilot_<os>_<arch>` from the latest release.
# That name is a contract shared with `pilot upgrade` and
# .github/workflows/release-pilot.yml; changing it in one place breaks the
# other two.
set -eu

REPO="${PILOT_REPO:-vivek7405/pilots}"
# Where it lands. ~/.local/bin because it needs no privilege and is on PATH in
# every modern distribution's default profile.
BIN_DIR="${PILOT_BIN_DIR:-$HOME/.local/bin}"
API="https://api.github.com/repos/$REPO/releases/latest"

say() { printf '%s\n' "$*"; }
die() { printf 'install-pilot: %s\n' "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required"; }

# One of the two, rather than both: curl is everywhere except Debian minimal,
# where wget is.
if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL "$1"; }
  fetch_to() { curl -fsSL -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -qO- "$1"; }
  fetch_to() { wget -qO "$2" "$1"; }
else
  die "curl or wget is required"
fi

os="$(uname -s)"
case "$os" in
  Linux) goos=linux ;;
  Darwin) goos=darwin ;;
  *) die "unsupported operating system: $os. Build from source: go build ./apps/pilot/cmd/pilot" ;;
esac

arch="$(uname -m)"
case "$arch" in
  x86_64 | amd64) goarch=amd64 ;;
  aarch64 | arm64) goarch=arm64 ;;
  *) die "unsupported architecture: $arch. Build from source: go build ./apps/pilot/cmd/pilot" ;;
esac

asset="pilot_${goos}_${goarch}"
say "install-pilot: looking for $asset"

# The download URL for that exact asset, read out of the release JSON without
# a JSON parser: jq is not installed on a fresh box and requiring it would
# defeat the point of a one-line install.
url="$(fetch "$API" \
  | tr ',' '\n' \
  | grep '"browser_download_url"' \
  | sed 's/.*"browser_download_url": *"\([^"]*\)".*/\1/' \
  | grep "/${asset}$" \
  | head -n 1)"
[ -n "$url" ] || die "the latest release of $REPO carries no $asset"

need mkdir
mkdir -p "$BIN_DIR"
# The temp file goes in BIN_DIR, not $TMPDIR. /tmp is usually a separate
# filesystem (a tmpfs on most Linux), and a cross-device `mv` is a copy
# followed by an unlink -- not a rename, so a killed install CAN leave a
# half-written pilot on PATH, which is the exact thing the comment below
# promised it could not. Same filesystem makes the rename real.
tmp="$(mktemp "$BIN_DIR/.pilot.XXXXXX")"
# Cleaned up on every exit path, including a failed download, so a half file
# is never left behind for someone to find and run.
trap 'rm -f "$tmp"' EXIT INT TERM

say "install-pilot: downloading $url"
fetch_to "$url" "$tmp"

# Verify against the checksums.txt the release publishes beside the binary.
#
# This script is run as `curl ... | sh`, so it fetches an executable over the
# network and puts it on PATH. The release workflow already writes
# checksums.txt; not reading it meant a corrupted download -- or a tampered
# one -- was installed and run with nothing noticing.
#
# A machine with neither sha256sum nor shasum says so rather than failing:
# refusing to install on a box that cannot hash would be a worse outcome than
# an unverified install the operator was told about.
sums_url="${url%/*}/checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
  sha256_of() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha256_of() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  sha256_of() { printf ''; }
fi

want="$(fetch "$sums_url" 2>/dev/null | grep " \*\?${asset}\$" | cut -d' ' -f1 | head -n 1)"
got="$(sha256_of "$tmp")"
if [ -z "$got" ]; then
  say "install-pilot: neither sha256sum nor shasum is installed; SKIPPING verification"
elif [ -z "$want" ]; then
  say "install-pilot: the release publishes no checksum for $asset; SKIPPING verification"
elif [ "$want" != "$got" ]; then
  die "checksum mismatch for $asset
  expected $want
  got      $got
The download is corrupt or has been tampered with; nothing was installed."
else
  say "install-pilot: sha256 verified"
fi

chmod 0755 "$tmp"

# Renamed into place rather than written in place: a half-written file is
# never the thing on PATH, which is the same rule `pilot upgrade` follows.
mv "$tmp" "$BIN_DIR/pilot"
trap - EXIT INT TERM

say "install-pilot: installed $("$BIN_DIR/pilot" version)"

case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *)
    say ""
    say "$BIN_DIR is not on your PATH. Add it:"
    say "  export PATH=\"\$PATH:$BIN_DIR\""
    ;;
esac

say ""
say "Next: pilot login, then pilot mcp install <harness> to connect your agent."
