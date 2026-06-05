#!/usr/bin/env bash
# Rebuild internal/web/static/app.css from the current templates with the pinned
# Tailwind CLI. Run after changing Tailwind classes in index.html or app.js.
# The generated file is committed and is what `go build` embeds, so the browser
# never compiles CSS at runtime.
set -euo pipefail

TAILWIND_VERSION=3.4.17

root="$(cd "$(dirname "$0")/.." && pwd)"
"$root/hack/get-tailwind.sh" "$TAILWIND_VERSION"
"$root/bin/tailwindcss" \
  -c "$root/hack/tailwind.config.js" \
  -i "$root/hack/tailwind.css" \
  -o "$root/internal/web/static/app.css" \
  --minify
echo "wrote internal/web/static/app.css"
