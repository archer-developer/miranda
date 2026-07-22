#!/usr/bin/env bash
# Downloads the Tailwind CSS v4 standalone CLI (no Node/npm required) into
# ./bin/tailwindcss, matching the current OS/arch. This is a build-time-only
# tool: the binary itself is gitignored, only the CSS it generates
# (internal/webui/static/css/styles.css) is committed and embedded into the
# Go binary.
set -euo pipefail
cd "$(dirname "$0")/.."

os="$(uname -s)"
arch="$(uname -m)"

case "$os" in
  Darwin) platform="macos" ;;
  Linux) platform="linux" ;;
  *) echo "unsupported OS: $os" >&2; exit 1 ;;
esac

case "$arch" in
  arm64|aarch64) platform_arch="${platform}-arm64" ;;
  x86_64|amd64) platform_arch="${platform}-x64" ;;
  *) echo "unsupported arch: $arch" >&2; exit 1 ;;
esac

mkdir -p bin
url="https://github.com/tailwindlabs/tailwindcss/releases/latest/download/tailwindcss-${platform_arch}"
echo "Downloading $url"
curl -sSL -o bin/tailwindcss "$url"
chmod +x bin/tailwindcss
bin/tailwindcss --help >/dev/null
echo "Installed bin/tailwindcss"
