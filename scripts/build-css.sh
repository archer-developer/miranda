#!/usr/bin/env bash
# Regenerates internal/webui/static/css/styles.css from the Tailwind classes
# used in internal/webui/templates and internal/webui/static/app.js. Run this
# after changing UI markup; the compiled output is committed since go:embed
# needs it present at `go build` time and we don't want a Node/npm build
# step in the runtime build path.
set -euo pipefail
cd "$(dirname "$0")/.."

bin="./bin/tailwindcss"
if [ ! -x "$bin" ]; then
  echo "bin/tailwindcss not found — run scripts/download-tailwindcli.sh first" >&2
  exit 1
fi

"$bin" -i internal/webui/static/css/input.css -o internal/webui/static/css/styles.css --minify
