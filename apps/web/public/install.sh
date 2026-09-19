#!/bin/sh
# Install the `pilot` CLI.
#
#   curl -fsSL https://pilots.run/install.sh | sh
#
# This file IS that URL (apps/web/app/install.sh/route.ts serves it). It lives
# in apps/web/public because the site's image holds apps/web and nothing else
# of the repository, and there is one copy because a second would be a second
# copy of the asset-name contract below.
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

REPO="${PILOT_REPO:-pilotsrun/pilots}"
# Where it lands. ~/.local/bin because it needs no privilege and is on PATH in
# every modern distribution's default profile.
BIN_DIR="${PILOT_BIN_DIR:-$HOME/.local/bin}"
API="https://api.github.com/repos/$REPO/releases/latest"

say() { printf '%s\n' "$*"; }
die() { printf 'pilot: %s\n' "$*" >&2; exit 1; }

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
say "pilot: looking for $asset"

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

# Verify against the checksums.txt the release publishes beside the binary,
# or install nothing.
#
# This script is run as `curl ... | sh`, so it fetches an executable over the
# network and puts it on PATH. There is ONE outcome that installs: the digest
# was read, the file was hashed, and the two agree. No hashing tool, a
# checksums.txt that cannot be fetched, and one with no line for this asset
# are each a refusal, which is what "verified" on the install page promises
# and what `pilot upgrade` does in the same cases. sprites' installer, the
# nearest prior art, dies on all three as well.
#
# All three are settled BEFORE the binary is downloaded: none of them needs
# it, and a refusal should cost one small request, not the whole download.
#
# A machine with neither tool is rare (coreutils and BusyBox ship sha256sum,
# macOS ships shasum) and is told what to install, or where to get the binary
# by hand.
sums_url="${url%/*}/checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then
  sha256_of() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha256_of() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  die "sha256sum or shasum is required to verify the download; nothing was installed.
Install one (coreutils), or take the binary from https://github.com/$REPO/releases"
fi

# A line is the digest, then the name, which binary mode prefixes with `*`.
# Matched as awk fields rather than with grep: the optional `*` would need
# `\?`, which POSIX leaves undefined in a basic regex, and a grep that reads it
# literally would refuse every install. It is the rule checksumFor follows in
# apps/pilot/internal/cli/upgrade.go.
need awk
sums="$(fetch "$sums_url")" || die "could not fetch $sums_url, so the download cannot be verified; nothing was installed"
want="$(printf '%s\n' "$sums" | awk -v a="$asset" '$2 == a || $2 == "*" a { print $1; exit }')"
[ -n "$want" ] || die "$sums_url carries no checksum for $asset, so the download cannot be verified; nothing was installed"

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

say "pilot: downloading $url"
fetch_to "$url" "$tmp"

got="$(sha256_of "$tmp")"
if [ "$want" != "$got" ]; then
  die "checksum mismatch for $asset
  expected $want
  got      $got
The download is corrupt or has been tampered with; nothing was installed."
fi
say "pilot: sha256 verified"

chmod 0755 "$tmp"

# Renamed into place rather than written in place: a half-written file is
# never the thing on PATH, which is the same rule `pilot upgrade` follows.
mv "$tmp" "$BIN_DIR/pilot"
trap - EXIT INT TERM

say "pilot: installed $("$BIN_DIR/pilot" version)"

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
