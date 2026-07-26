---
name: deploy
description: Build Miranda for linux/amd64 and ship it to the production server (archer@192.168.1.50) over SSH, then restart the systemd --user service. Use when the user asks to deploy, release, ship, push to the server, or restart the remote miranda process.
---

# Deploying Miranda

Miranda ships as a single static Go binary (`CGO_ENABLED=0`, no cgo, no
Docker — see `CLAUDE.md`). Deploying means: cross-compile it for the
server's architecture, copy it over SSH, and restart the process that runs
it.

## What this does

Run `scripts/deploy.sh` from the repo root. It:

1. Builds `dist/miranda-linux-amd64` with `GOOS=linux GOARCH=amd64 CGO_ENABLED=0`.
2. Uploads the binary to `archer@192.168.1.50:~/miranda/miranda.new`, then
   `mv`s it into place on the server (atomic rename, not a truncate-in-place
   write — safe even while the old binary is still running).
3. Generates and uploads a `systemd --user` unit
   (`~/.config/systemd/user/miranda.service`) from a template in the script,
   so the unit is always in sync with the script rather than hand-maintained
   on the server.
4. Runs `systemctl --user daemon-reload` and `restart miranda`, then prints
   `systemctl --user status` and polls `http://localhost:8787/healthz` on
   the server to confirm the new process actually came up.

It never touches `config.yaml`, `data/`, `logs/`, or `.env` on the
server — those hold server-specific secrets and state and are managed
separately, outside this skill.

```bash
./scripts/deploy.sh
```

## Before running

- This talks to a real production server over SSH and restarts a live
  process — run it when the user has actually asked to deploy, not
  speculatively. If invoked outside an explicit "deploy"/"release" request,
  confirm with the user first.
- Make sure the working tree is in the state the user wants shipped (check
  `git status`; ask before deploying uncommitted or unpushed changes if
  that seems unintentional).
- SSH auth to `archer@192.168.1.50` must already work key-based
  (`ssh archer@192.168.1.50` with no password prompt) — this skill doesn't
  set that up.

## One-time server setup (not part of every deploy)

`systemd --user` services stop when the user's last login session ends
unless lingering is enabled. The deploy script detects and warns about this
but can't fix it remotely without a password prompt. If the warning
appears, run once:

```bash
ssh archer@192.168.1.50 loginctl enable-linger archer
```

## If something goes wrong

- Build fails locally: fix it like any other Go build failure — the deploy
  script doesn't run anything remote until the binary compiles.
- `healthz` check fails after restart: SSH in and check the unit's own
  logs, since stdout/stderr under systemd go to the journal, not
  `logs/miranda.log` (that file only gets Miranda's own app-level log
  mirror, not startup crashes):
  ```bash
  ssh archer@192.168.1.50 journalctl --user -u miranda -n 100 --no-pager
  ```
- To roll back, re-run this skill after `git checkout` to the last-good
  commit — there's no separate rollback path; redeploying an old commit is
  the rollback.