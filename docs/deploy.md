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
It has to name the port that serves the API. Where a public `/hooks` split moves the API to
another port, a runner left on the old one logs a 404 on the WebSocket handshake and retries
forever, while its jobs sit in `queued` with nothing said about it. A runner beside the server
can skip the tailnet with `http://127.0.0.1:8080`.
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

Job sessions run with `--strict-mcp-config` and, by default, an empty MCP configuration: MCP
servers and claude.ai connectors attached to the logged-in account (Linear, Google Drive, social
tools…) are never reachable from a job, even with `bypassPermissions`. Grant an explicit set with
`GATOR_RUNNER_CLAUDE_MCP_CONFIG`. Each job's `session` event lists the MCP servers it had.
Prefer a Claude account dedicated to runners over a personal one.

The runner logs each backend's login state at start: `ok`, `missing` (the command is there, the
credentials are not), `no_cli` (the command is not on the runner's PATH at all) or `unknown`
(the runner could not tell). On macOS the login lives in the Keychain; the runner asks whether
the entry exists, which returns its attributes and does not prompt — reading the secret would,
and it never does that. A locked keychain or a missing `security` command leaves `unknown`. `no_cli` is
the one that catches people out on a Mac: launchd hands an agent a nearly empty PATH, and Claude
Code installs itself in `~/.local/bin`, so a machine whose owner is signed in can still report
that its backend is not there. The app's agent puts `~/.local/bin` on the PATH; a hand-written
plist should too.

Signing a backend in happens **on the runner's own machine, as the user the runner runs as** —
`claude` in a terminal there. The runner reads `~/.claude/.credentials.json`, or
`ANTHROPIC_API_KEY` / `CLAUDE_CODE_OAUTH_TOKEN` from its environment. No runner can be driven
through an interactive login from the app today: runners say so in `capabilities.canLogin`, and
`POST /runners/{id}/login` is refused with 409 rather than accepted and dropped.

To push job branches the runner user needs git access to the project's repositories, for
example a deploy key with write access or a fine-grained token in its git credential helper.
`GATOR_RUNNER_PUSH=0` keeps branches in the runner's cache instead.

The runner user is what agents run as. A worktree is isolation, not a sandbox: give that user
no credentials beyond what its jobs need.

### Push notifications

Apple push needs a key from the Apple Developer Program: **Certificates, Identifiers &
Profiles → Keys → +**, tick **Apple Push Notifications service**, configure it for Sandbox &
Production, and download the `.p8` — Apple hands it over exactly once. Each app id it serves
needs the Push Notifications capability.

```sh
sudo install -o gator -g gator -m 400 AuthKey_XXXXXXXXXX.p8 /etc/gator/apns.p8
sudoedit /etc/gator/server.env   # GATOR_APNS_KEY=/etc/gator/apns.p8
                                 # GATOR_APNS_KEY_ID=XXXXXXXXXX   (in the file name)
                                 # GATOR_APNS_TEAM_ID=YYYYYYYYYY  (top right in the portal)
                                 # GATOR_APNS_TOPIC=dev.onegator.gator
                                 # GATOR_APNS_TOPIC_IOS=dev.onegator.gator.ios
sudo systemctl restart gator-server
```

The four `GATOR_APNS_*` settings are read together or not at all; without them the server keeps
the disabled sender and logs `notifications sender=disabled`. Apple routes by bundle id, so set
`GATOR_APNS_TOPIC_IOS` whenever the iOS app has its own: one topic for both platforms earns
`DeviceTokenNotForTopic`, which the server reads as a dead device and forgets the token.
`GATOR_APNS_SANDBOX=1` switches to Apple's test servers. The key is a secret; the key and team
ids are not.

macOS only accepts push for an app signed with a Developer ID and notarised, so a locally built
Gator.app registers no token until it is signed.

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

Gator.app carries `gator-runner` inside its own bundle and installs it as a LaunchAgent when
you turn on **This Mac is a runner** on the Runners screen: it mints the Mac a runner token of
its own, writes the token to `~/Library/Application Support/Gator/runner-token` (mode 600) and
the agent to `~/Library/LaunchAgents/dev.onegator.gator.runner.plist`, which names that file in
`GATOR_RUNNER_TOKEN_FILE` rather than carrying the secret. Turning the switch off boots the
agent out, deletes both and forgets the runner on the server, which revokes its token — a Mac
that is gone leaves no credentials behind. Any runner can also be forgotten from the Runners
screen once it is disconnected: it leaves the list and its token stops working, while the jobs
it ran keep pointing at it.

`deploy/launchd/dev.onegator.gator-runner.plist` is the same agent by hand, for a Mac without
the app. Either way a sleeping Mac or a logged-out user shows as offline and its leases return
to the queue — a state, not a fault.

## Backups and restore

With `GATOR_BACKUP_S3_*` set the server uploads a `pg_dump` every hour and keeps the newest
`GATOR_BACKUP_KEEP`. `gator-server backup now` runs one immediately.
`gator-server restore <file>` replays a dump. The `restore-drill` workflow proves this weekly.
