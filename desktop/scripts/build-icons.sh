#!/usr/bin/env bash
# Rebuild the Agent Hub icon set from src-tauri/icons/icon.svg.
#
# The master is vector-only, so every raster comes from a headless Chromium-family
# browser: that is the same renderer that drew the gradients, the clipped sheen and
# the glyph shadow, and it rasterises each size natively instead of downscaling one
# big bitmap. macOS ships `sips` and `iconutil`; ImageMagick is optional and only
# needed for the Windows `.ico`.
#
# Set BROWSER=/path/to/chrome to pick the browser explicitly.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
icons="$root/src-tauri/icons"
master="$icons/icon.svg"

if [ ! -f "$master" ]; then
  echo "missing icon master: $master" >&2
  exit 1
fi

browser=${BROWSER:-}
if [ -z "$browser" ]; then
  for candidate in \
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
    "/Applications/Chromium.app/Contents/MacOS/Chromium" \
    "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge" \
    "/Applications/Brave Browser.app/Contents/MacOS/Brave Browser"; do
    if [ -x "$candidate" ]; then
      browser=$candidate
      break
    fi
  done
fi
if [ -z "$browser" ]; then
  echo "no Chromium-family browser found; set BROWSER=/path/to/chrome" >&2
  exit 1
fi

work=$(mktemp -d "${TMPDIR:-/tmp}/agenthub-icons.XXXXXX")
trap 'rm -rf "$work"' EXIT

# render <size> <output.png>
#
# One browser launch per size: the page is a single <img> at exact pixel dimensions
# on a transparent background, so the SVG is rasterised at that size rather than
# resampled from 1024.
render() {
  local size=$1 out=$2
  cat > "$work/page.html" <<HTML
<!doctype html><meta charset="utf-8">
<body style="margin:0;background:transparent">
<img src="file://$master" width="$size" height="$size" style="display:block">
HTML
  "$browser" \
    --headless=new --disable-gpu --hide-scrollbars --disable-extensions \
    --no-first-run --no-default-browser-check \
    --user-data-dir="$work/profile" --force-device-scale-factor=1 \
    --window-size="$size,$size" --default-background-color=00000000 \
    --screenshot="$out" "file://$work/page.html" >"$work/browser.log" 2>&1 || {
      cat "$work/browser.log" >&2
      echo "render failed at ${size}px" >&2
      exit 1
    }
}

for size in 16 32 48 64 128 256 512 1024; do
  render "$size" "$work/$size.png"
done

# Tauri's cross-platform set. `icon.png` is the 1024 master used for Linux bundles
# and as the window-icon fallback.
cp "$work/1024.png" "$icons/icon.png"
cp "$work/32.png" "$icons/32x32.png"
cp "$work/64.png" "$icons/64x64.png"
cp "$work/128.png" "$icons/128x128.png"
cp "$work/256.png" "$icons/128x128@2x.png"

# macOS .icns: ten named slots, which is why several sizes appear twice.
iconset="$work/AgentHub.iconset"
mkdir -p "$iconset"
cp "$work/16.png" "$iconset/icon_16x16.png"
cp "$work/32.png" "$iconset/icon_16x16@2x.png"
cp "$work/32.png" "$iconset/icon_32x32.png"
cp "$work/64.png" "$iconset/icon_32x32@2x.png"
cp "$work/128.png" "$iconset/icon_128x128.png"
cp "$work/256.png" "$iconset/icon_128x128@2x.png"
cp "$work/256.png" "$iconset/icon_256x256.png"
cp "$work/512.png" "$iconset/icon_256x256@2x.png"
cp "$work/512.png" "$iconset/icon_512x512.png"
cp "$work/1024.png" "$iconset/icon_512x512@2x.png"
iconutil -c icns "$iconset" -o "$icons/icon.icns"

# Windows .ico. Optional: this app is macOS-first, and ImageMagick is not part of
# the checked toolchain, so a missing magick keeps whatever .ico is already there.
if command -v magick >/dev/null 2>&1; then
  magick "$work/16.png" "$work/32.png" "$work/48.png" "$work/64.png" \
    "$work/128.png" "$work/256.png" "$icons/icon.ico"
else
  echo "note: ImageMagick not found, icon.ico not regenerated" >&2
fi

echo "icons rebuilt in $icons"
