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

### Before OIDC is configured

The first admin and runner tokens can be created on the host, with database access instead
of an API token:

```sh
sudo /usr/local/lib/gator/install.sh exec admin create-admin you@example.com "Your Name"
sudo /usr/local/lib/gator/install.sh exec admin issue-runner-token vps-1
```

Each token is printed once. The admin account links to OIDC on its first login with the same
verified email; an email already linked to another identity is refused.

Runner on the same or another host:

```sh
sudo sh install.sh install runner          # user `gator-runner`, /etc/gator/runner.env, unit
sudoedit /etc/gator/runner.env             # server base URL, runner token, backends
sudo -iu gator-runner claude               # log Claude Code in as the runner user, then /exit
sudo systemctl start gator-runner
```

Mint a runner token as a workspace admin with `POST /api/v1/tokens` and
`{"kind":"runner","name":"vps-1"}`, or on the server host with
`install.sh exec admin issue-runner-token vps-1`. The token is shown once.

`GATOR_RUNNER_SERVER_URL` is the server's base URL, for example
`https://gator.<tailnet>.ts.net`; the runner switches to `wss` and appends `/api/v1/runner`.
`GATOR_RUNNER_BACKENDS` lists the agent CLIs this runner can drive (`claude`, `codex`). A runner
with no backends connects and shows as online but is never handed a job.

### Agent backends

Install Claude Code for the runner user (it lands in its home, which the unit keeps writable),
log it in once interactively, then enable the backend:

```sh
sudo -iu gator-runner sh -c 'curl -fsSL https://claude.ai/install.sh | bash'
sudo -iu gator-runner ~/.local/bin/claude          # log in, then /exit
sudoedit /etc/gator/runner.env                     # GATOR_RUNNER_BACKENDS=claude
                                                   # GATOR_RUNNER_CLAUDE_BIN=/var/lib/gator-runner/.local/bin/claude
sudo systemctl restart gator-runner
```

The runner logs each backend's login state at start (`ok`, `missing`, or `unknown` on macOS,
where the login lives in the Keychain).

To push job branches the runner user needs git access to the project's repositories, for
example a deploy key with write access or a fine-grained token in its git credential helper.
`GATOR_RUNNER_PUSH=0` keeps branches in the runner's cache instead.

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
