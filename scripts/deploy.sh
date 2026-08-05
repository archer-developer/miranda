#!/usr/bin/env bash
# Builds miranda for linux/amd64, ships it to the server over SSH, and
# restarts the systemd --user service. Only the binary and the systemd unit
# file are touched on the server — config.yaml, data/, logs/, and .env are
# never uploaded or overwritten, since they hold server-specific state
# (secrets, SQLite DBs, per-user memory).
set -euo pipefail
cd "$(dirname "$0")/.."

remote_host="archer@miranda"
remote_dir="miranda"           # relative to the SSH user's $HOME
service_name="miranda"
build_out="dist/miranda-linux-amd64"
unit_file="$(mktemp)"
trap 'rm -f "$unit_file"' EXIT

echo "==> Building $build_out (GOOS=linux GOARCH=amd64, CGO_ENABLED=0)"
mkdir -p dist
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$build_out" ./cmd/miranda

# %h expands to the systemd --user service's own $HOME on the target host,
# so this unit never has to hardcode a path that might differ per server.
cat >"$unit_file" <<EOF
[Unit]
Description=Miranda home voice assistant agent service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=%h/${remote_dir}
ExecStart=%h/${remote_dir}/${service_name}
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
EOF

echo "==> Uploading binary and systemd unit to ${remote_host}:~/${remote_dir}"
ssh "$remote_host" "mkdir -p ~/${remote_dir} ~/.config/systemd/user"
scp -q "$build_out" "${remote_host}:~/${remote_dir}/${service_name}.new"
scp -q "$unit_file" "${remote_host}:~/.config/systemd/user/${service_name}.service"

echo "==> Installing and restarting on ${remote_host}"
ssh "$remote_host" bash -s -- "$remote_dir" "$service_name" <<'REMOTE'
set -euo pipefail
remote_dir="$1"
service_name="$2"

chmod +x ~/"$remote_dir"/"$service_name".new
mv ~/"$remote_dir"/"$service_name".new ~/"$remote_dir"/"$service_name"

systemctl --user daemon-reload
systemctl --user enable --now "$service_name" >/dev/null
systemctl --user restart "$service_name"

if [ "$(loginctl show-user "$(whoami)" --property=Linger --value 2>/dev/null)" != "yes" ]; then
  echo "WARNING: lingering is not enabled for this user — the service will" >&2
  echo "  stop when the SSH session ends. Run once: loginctl enable-linger $(whoami)" >&2
fi

echo "--- systemctl --user status $service_name ---"
systemctl --user --no-pager -l status "$service_name" || true

echo "--- health check ---"
for i in 1 2 3 4 5; do
  if curl -fsS -m 2 http://localhost:8787/healthz; then
    echo
    exit 0
  fi
  sleep 1
done
echo "healthz check failed after restart" >&2
exit 1
REMOTE

echo "==> Deploy complete"
