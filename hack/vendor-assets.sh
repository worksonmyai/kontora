#!/usr/bin/env bash
# Re-vendor every third-party web asset into internal/web/static/vendor and
# rebuild internal/web/static/app.css. Run this when bumping a version below;
# commit the result. This script is never part of `go build` itself, so a plain
# `go build` / `go install` stays offline and reproducible.
#
# To bump a library: change its version here, run `make assets`, then update the
# matching /vendor/<name>@<version>/ paths in static/index.html (and app.js for
# xterm). The versioned directory makes the old/new swap obvious in the diff.
set -euo pipefail

# --- pinned versions ---------------------------------------------------------
ALPINE=3.14.8
SORTABLE=1.15.6
MARKED=15.0.7
DOMPURIFY=3.3.2
XTERM=5.5.0
ADDON_FIT=0.10.0
ADDON_UNICODE11=0.8.0
# fonts: DM Sans 400..600 + JetBrains Mono 400..700, latin + latin-ext subsets.
# Tailwind CLI version lives in hack/build-css.sh.

root="$(cd "$(dirname "$0")/.." && pwd)"
vendor="$root/internal/web/static/vendor"
jsd="https://cdn.jsdelivr.net/npm"

fetch() { mkdir -p "$(dirname "$2")"; echo "  $2"; curl -fsSL "$1" -o "$2"; }

echo "vendoring js/css libs..."
fetch "$jsd/alpinejs@$ALPINE/dist/cdn.min.js"             "$vendor/alpinejs@$ALPINE/cdn.min.js"
fetch "$jsd/sortablejs@$SORTABLE/Sortable.min.js"         "$vendor/sortablejs@$SORTABLE/Sortable.min.js"
fetch "$jsd/marked@$MARKED/marked.min.js"                 "$vendor/marked@$MARKED/marked.min.js"
fetch "$jsd/dompurify@$DOMPURIFY/dist/purify.min.js"      "$vendor/dompurify@$DOMPURIFY/purify.min.js"
fetch "$jsd/@xterm/xterm@$XTERM/css/xterm.css"            "$vendor/xterm@$XTERM/xterm.css"
fetch "$jsd/@xterm/xterm@$XTERM/+esm"                     "$vendor/xterm@$XTERM/xterm.mjs"
fetch "$jsd/@xterm/addon-fit@$ADDON_FIT/+esm"             "$vendor/addon-fit@$ADDON_FIT/addon-fit.mjs"
fetch "$jsd/@xterm/addon-unicode11@$ADDON_UNICODE11/+esm" "$vendor/addon-unicode11@$ADDON_UNICODE11/addon-unicode11.mjs"

echo "vendoring fonts..."
mkdir -p "$vendor/fonts"
raw="$(mktemp)"
ua='Mozilla/5.0 AppleWebKit/537.36 Chrome/124.0 Safari/537.36'
curl -fsSL -A "$ua" -G \
  --data-urlencode 'family=DM Sans:wght@400..600' \
  --data-urlencode 'family=JetBrains Mono:wght@400..700' \
  --data-urlencode 'display=swap' \
  'https://fonts.googleapis.com/css2' -o "$raw"
python3 "$root/hack/build_fonts.py" "$raw" "$vendor/fonts"
rm -f "$raw"

echo "building app.css..."
"$root/hack/build-css.sh"

echo "done. review changes with: git status internal/web/static"
