#!/bin/sh
# Builds the GitHub Pages demo into _site/: the real UI from
# internal/web/static plus demo/mock-api.js, an in-browser fake backend
# that loads before app.js. Used by the Pages workflow and `task demo`.
set -eu
cd "$(dirname "$0")/.."
rm -rf _site
mkdir -p _site
cp -R internal/web/static/. _site/
cp demo/mock-api.js _site/mock-api.js
touch _site/.nojekyll
# Load the fake backend first (not deferred), so it's in place before app.js runs.
sed 's#<script src="app.js" defer></script>#<script src="mock-api.js"></script>\n  <script src="app.js" defer></script>#' internal/web/static/index.html > _site/index.html
sed -i 's#<title>YourSix IFTTT</title>#<title>YourSix IFTTT · demo</title>#' _site/index.html
grep -q 'src="mock-api.js"' _site/index.html || { echo "demo/build.sh: failed to inject mock-api.js" >&2; exit 1; }
echo "built _site/ ($(ls _site | wc -l | tr -d ' ') files)"
