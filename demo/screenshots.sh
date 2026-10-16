#!/bin/sh
# Retakes the README screenshots (docs/screenshots/*-light.png and
# *-dark.png) from the demo build, using a headless Chromium browser (Chrome,
# Chromium or Edge; set BROWSER to its path if it isn't found). Each shot is
# 1200 CSS pixels wide at 2x. demo/screenshots.js sets up the screen named
# by ?shot=. Used by `task screenshots`.
set -eu
cd "$(dirname "$0")/.."
sh demo/build.sh >/dev/null

rm -rf _shots
cp -R _site _shots
sed -i 's#<script src="app.js" defer></script>#<script src="app.js" defer></script>\n  <script src="screenshots.js" defer></script>#' _shots/index.html
cp demo/screenshots.js _shots/screenshots.js

browser=${BROWSER:-}
if [ -z "$browser" ]; then
  for b in google-chrome chromium chromium-browser \
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
    "/c/Program Files/Google/Chrome/Application/chrome.exe" \
    "/c/Program Files (x86)/Microsoft/Edge/Application/msedge.exe"; do
    if command -v "$b" >/dev/null 2>&1 || [ -x "$b" ]; then browser=$b; break; fi
  done
fi
[ -n "$browser" ] || { echo "demo/screenshots.sh: no Chrome, Chromium or Edge found; set BROWSER" >&2; exit 1; }

page="$PWD/_shots/index.html"
out="$PWD/docs/screenshots"
if command -v cygpath >/dev/null 2>&1; then page=$(cygpath -m "$page"); out=$(cygpath -m "$out"); fi
page=$(printf "%s" "$page" | sed "s/ /%20/g")

# name:height (CSS pixels) for each shot.
for spec in rules:860 events:880 editor:1560 actions:900 schedules:760 schedule-editor:1180 history:960 secrets:560 setup:820; do
  name=${spec%%:*}
  height=${spec#*:}
  for theme in light dark; do
    # Blink's preferred color scheme: 0 = dark, 1 = light. Set both ways so
    # the system theme doesn't leak into the light shots.
    scheme=1
    [ "$theme" = dark ] && scheme=0
    "$browser" --headless=new --disable-gpu --hide-scrollbars --force-device-scale-factor=2 \
      --window-size=1200,"$height" --virtual-time-budget=8000 --blink-settings=preferredColorScheme=$scheme \
      --allow-file-access-from-files \
      --screenshot="$out/$name-$theme.png" "file:///${page#/}?shot=$name" >/dev/null 2>&1
    echo "docs/screenshots/$name-$theme.png"
  done
done
rm -rf _shots
