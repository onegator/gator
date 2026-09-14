# Deploying gator

Three pieces: **gator-server** (the brain, one per team), **gator-runner** on a VPS
(always on) and optionally **gator-runner** on a Mac (while it is awake). PostgreSQL 16+ is
the only dependency of the server.

## Network shape

- The server listens on `127.0.0.1:8080`.
- People and runners reach it over **Tailscale**: `tailscale serve --bg 8080` gives
  `https://<host>.<tailnet>.ts.net` with a certificate, including the WebSocket.
- Only plugin webhooks are public: Caddy (`deploy/caddy/Caddyfile`) forwards `/hooks/*` and
  answers 404 for everything else.

## VPS with systemd

```sh
curl -fsSLO https://github.com/onegator/gator/releases/latest/download/install.sh
sudo sh install.sh install server         # binary, user `gator`, /etc/gator/server.env, unit
sudoedit /etc/gator/server.env             # database URL, OIDC, backups
sudo /usr/local/lib/gator/install.sh migrate
sudo systemctl start gator-server
```

`install` generates `GATOR_SECRETS_KEY` on first run. Store a copy outside the host: without
it encrypted secrets cannot be read after a restore.

Runner on the same or another host:

```sh
sudo sh install.sh install runner          # user `gator-runner`, /etc/gator/runner.env, unit
sudoedit /etc/gator/runner.env             # server URL and a runner token
sudo -iu gator-runner claude login         # log in the agent CLIs as the runner user
sudo systemctl start gator-runner
```

Mint a runner token as a workspace admin with `POST /api/v1/tokens` and
`{"kind":"runner","name":"vps-1"}`. The token is shown once.

The runner user is what agents run as. A worktree is isolation, not a sandbox: give that user
no credentials beyond what its jobs need.

### Upgrades

Run `install` again with the new version. It replaces the binary, keeps env files and
restarts a running unit. Then apply migrations if the release notes mention any:

```sh
sudo sh install.sh install server --version v0.2.0
sudo /usr/local/lib/gator/install.sh migrate
```

Migrations never run on startup, and every migration has a down step
(`gator-server migrate-down`).

## Docker

```sh
cp deploy/server.env.example deploy/server.env && chmod 600 deploy/server.env
POSTGRES_PASSWORD=... docker compose -f deploy/compose.yaml up -d
```

The `migrate` service runs once before `server` starts. Pin `GATOR_IMAGE_TAG` in production.

## Mac runner

`deploy/launchd/dev.onegator.gator-runner.plist` runs `gator-runner` as a LaunchAgent. Gator.app
will install and manage it (PLQ-230). A sleeping Mac shows as offline and its leases return
to the queue.

## Backups and restore

With `GATOR_BACKUP_S3_*` set the server uploads a `pg_dump` every hour and keeps the newest
`GATOR_BACKUP_KEEP`. `gator-server backup now` runs one immediately.
`gator-server restore <file>` replays a dump. The `restore-drill` workflow proves this weekly.
