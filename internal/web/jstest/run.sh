#!/bin/sh
# Runs the browser-side logic tests (logic.test.js against static/logic.js)
# with Node if it's installed, otherwise in a headless Chrome, Chromium or
# Edge (set BROWSER to its path if it isn't found). Used by `task test:js`.
set -eu
cd "$(dirname "$0")"

if command -v node >/dev/null 2>&1; then
  exec node run-node.mjs
fi

browser=${BROWSER:-}
if [ -z "$browser" ]; then
  for b in google-chrome chromium chromium-browser \
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" \
    "/c/Program Files/Google/Chrome/Application/chrome.exe" \
    "/c/Program Files (x86)/Microsoft/Edge/Application/msedge.exe"; do
    if command -v "$b" >/dev/null 2>&1 || [ -x "$b" ]; then browser=$b; break; fi
  done
fi
[ -n "$browser" ] || { echo "jstest: install Node, or Chrome/Chromium/Edge (or set BROWSER)" >&2; exit 1; }

page="$PWD/run.html"
if command -v cygpath >/dev/null 2>&1; then page=$(cygpath -m "$page"); fi
page=$(printf "%s" "$page" | sed "s/ /%20/g")
out=$("$browser" --headless=new --disable-gpu --allow-file-access-from-files --dump-dom "file:///${page#/}" 2>/dev/null)
# The results are the body's text, one line per test.
results=$(printf "%s\n" "$out" | sed -n '/<body id="results">/,/<\/body>/p' | sed 's/<[^>]*>//g' | sed '/^$/d')
[ -n "$results" ] || { echo "jstest: no results from $browser" >&2; exit 1; }
printf "%s\n" "$results"
if printf "%s\n" "$results" | grep -q '^FAIL'; then exit 1; fi
