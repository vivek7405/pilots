#!/usr/bin/env bash
# Bakes every raster favicon from public/favicon.svg, the one source.
#
#   favicon-192.png       the icon a crawler takes: square, and a multiple of 48
#   favicon.png           512, for anything that wants a large one
#   apple-touch-icon.png  180, full bleed and opaque: iOS rounds the corners
#                         itself and paints transparency black
#   favicon.ico           16, 32 and 48, served at /favicon.ico for clients that
#                         read no markup at all
#
# Needs rsvg-convert (librsvg) and magick (ImageMagick). The outputs are
# committed, so neither is needed to build or to run the tests.
set -euo pipefail

cd "$(dirname "$0")/../public"
src=favicon.svg
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

for tool in rsvg-convert magick; do
  command -v "$tool" >/dev/null || { echo "missing $tool" >&2; exit 1; }
done

rsvg-convert -w 192 -h 192 "$src" -o favicon-192.png
rsvg-convert -w 512 -h 512 "$src" -o favicon.png

# The Apple icon is the same drawing on a square tile with no border.
sed -E 's|<rect id="tile"[^>]*/>|<rect id="tile" width="32" height="32" fill="#0e1014"/>|' "$src" > "$tmp/apple.svg"
grep -q 'id="tile" width="32"' "$tmp/apple.svg" || { echo "the tile rect in $src moved" >&2; exit 1; }
rsvg-convert -w 180 -h 180 "$tmp/apple.svg" -o apple-touch-icon.png

for size in 16 32 48; do
  rsvg-convert -w "$size" -h "$size" "$src" -o "$tmp/$size.png"
done
magick "$tmp/16.png" "$tmp/32.png" "$tmp/48.png" favicon.ico

ls -l favicon.svg favicon-192.png favicon.png apple-touch-icon.png favicon.ico
