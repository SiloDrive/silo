#!/usr/bin/env bash
# Re-render the PNG/ICO set from the source SVGs. Needs rsvg-convert and magick.
set -euo pipefail
cd "$(dirname "$0")"
mkdir -p png

for s in 64 128 256 512; do
  rsvg-convert -w "$s" -h "$s" silo-mark-red.svg    -o "png/silo-mark-red-$s.png"
  rsvg-convert -w "$s" -h "$s" silo-mark-orange.svg -o "png/silo-mark-orange-$s.png"
done

for s in 128 256 512 1024; do
  rsvg-convert -w "$s" -h "$s" silo-app-icon-red.svg    -o "png/silo-app-icon-red-$s.png"
  rsvg-convert -w "$s" -h "$s" silo-app-icon-orange.svg -o "png/silo-app-icon-orange-$s.png"
done

rsvg-convert -w 180 -h 180 silo-app-icon-red.svg -o png/apple-touch-icon.png
rsvg-convert -w 16  -h 16  silo-16-red.svg -o png/favicon-16.png
rsvg-convert -w 32  -h 32  silo-32-red.svg -o png/favicon-32.png
rsvg-convert -w 48  -h 48  silo-32-red.svg -o png/favicon-48.png

magick png/favicon-16.png png/favicon-32.png png/favicon-48.png favicon.ico
echo "rendered $(ls png | wc -l | tr -d ' ') png + favicon.ico"
