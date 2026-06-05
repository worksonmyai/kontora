#!/usr/bin/env bash
# Download the pinned Tailwind v3 standalone CLI to bin/tailwindcss if it is not
# already present. The standalone binary bundles its own runtime, so no Node or
# npm is required. Usage: get-tailwind.sh <version>
set -euo pipefail

ver="${1:?version required}"
root="$(cd "$(dirname "$0")/.." && pwd)"
out="$root/bin/tailwindcss"

[ -x "$out" ] && exit 0

os="$(uname -s)"
arch="$(uname -m)"
case "$os" in
  Darwin) os=macos ;;
  Linux) os=linux ;;
  *) echo "unsupported OS: $os" >&2; exit 1 ;;
esac
case "$arch" in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64) arch=x64 ;;
  *) echo "unsupported arch: $arch" >&2; exit 1 ;;
esac

mkdir -p "$root/bin"
url="https://github.com/tailwindlabs/tailwindcss/releases/download/v${ver}/tailwindcss-${os}-${arch}"
echo "downloading $url"
curl -fsSL "$url" -o "$out"
chmod +x "$out"
# Clear the macOS quarantine bit so the freshly downloaded binary can run.
xattr -d com.apple.quarantine "$out" 2>/dev/null || true
