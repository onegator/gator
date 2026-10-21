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
no credentials beyond what its jobs need. See [What an agent can reach](#what-an-agent-can-reach).

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

`deploy/launchd/dev.onegator.gator.runner.plist` is the same agent by hand, for a Mac without
the app. Either way a sleeping Mac or a logged-out user shows as offline and its leases return
to the queue — a state, not a fault.

## Where a job runs

Every job has a **placement**: `local` or `cloud`. A runner takes only jobs of its own
placement; it says which with `GATOR_RUNNER_PLACEMENT`, or leaves it to its location (`vps` is
cloud, `mac` and `other` are local). The Runners screen shows each runner's placement.

- **local** (the default) runs on a machine of the person who started the job. A runner's owner
  is whoever minted its token — Gator.app mints one per Mac — and a local job someone starts
  goes to their own runner or to a shared one (a token minted on the host has no owner), never
  to another person's Mac, which is logged in as that person. Jobs the autopilot queues have
  nobody behind them and go to any local runner.
- **cloud** runs on a cloud runner: a long-lived one (a VPS with `GATOR_RUNNER_PLACEMENT=cloud`)
  or one the server starts for the job (below).

A person chooses per job — "On this Mac" or "In the cloud" in the task, or `placement` on
`POST /tasks/{id}/jobs`, which defaults to `local`. The autopilot uses the project's
`policy.placement` in its process configuration (`PUT /projects/{id}/config`), `local` unless
set to `cloud`.

Upgrading from a release without placements: a VPS runner that names no placement becomes a
cloud runner, and every job — the autopilot's included — is local unless chosen otherwise. If the
VPS is your only runner, either set `GATOR_RUNNER_PLACEMENT=local` on it to keep things as they
were, or set `policy.placement` to `cloud` on the projects it serves.

## Cloud runners on demand

With a provisioner configured, a cloud job that no connected cloud runner can take gets a
machine of its own. The server mints a runner token for that one job — it leases that job and
nothing else, reads nothing through the API and is revoked when the job ends — and starts the
runner image with `GATOR_RUNNER_ONCE=1`: the runner takes its job, delivers the receipt and
exits, and the machine goes with it. The server stops it as well once the job is over, replaces
a machine that has not taken its job within five minutes, and after three machines stops the
job with the reason. Without a provisioner, cloud jobs wait for a long-lived cloud runner and
show up as having no runner after two minutes.

Build the image from the repository root:

```sh
docker build -f Dockerfile.runner -t gator-runner:latest .
# pin the agent CLI: --build-arg CLAUDE_CODE_VERSION=2.1.270
```

It carries `gator-runner`, `gator-cli`, git, ssh and Claude Code, and runs as an unprivileged
user. Configure the server:

| Variable | |
|---|---|
| `GATOR_CLOUD_PROVISIONER` | `docker` or `fly`; empty starts nothing |
| `GATOR_CLOUD_RUNNER_IMAGE` | the image above, as the provisioner can pull it |
| `GATOR_CLOUD_SERVER_URL` | how a machine reaches this server, e.g. `http://host.docker.internal:8080` for a container next to a dev server, or the tailnet URL |
| `GATOR_CLOUD_RUNNER_ENV` | comma-separated names of server variables every machine gets — the backend's credentials. Nothing else of the server's environment leaves it, and `GATOR_*` names are refused |
| `GATOR_CLOUD_DOCKER_NETWORK`, `GATOR_CLOUD_DOCKER_ARGS` | docker only: the network to join, extra `docker run` arguments (`--cpus 2 --memory 4g`, `--add-host host.docker.internal:host-gateway` on Linux) |
| `FLY_API_TOKEN`, `FLY_APP`, `GATOR_CLOUD_FLY_REGION` | fly only |

**Docker** runs each machine with the docker CLI on the server's host, so `DOCKER_HOST` and
docker contexts apply: the local engine in development, or a VPS's engine over
`DOCKER_HOST=ssh://user@vps`. Values reach `docker run` through its environment and only their
names through `--env NAME`, so they never appear in a process list. The server needs the docker
CLI and access to the engine, which the server image does not have; run the server on the host
or give it a docker client and socket deliberately.

**Fly Machines** creates one machine per job through the Machines API (`auto_destroy`, no
restart, 2 shared CPUs and 2 GB) in an app you create once (`fly apps create gator-runners`) and
push the image to (`registry.fly.io/gator-runners:<tag>`). `FLY_API_TOKEN` should be a token
scoped to that app (`fly tokens create deploy -a gator-runners`). Machines start and bill per
second; nothing runs between jobs.

**Backend login.** A cloud machine has no keychain and nobody to log in interactively. Give it
a credential through the environment and name it in `GATOR_CLOUD_RUNNER_ENV`:

- `CLAUDE_CODE_OAUTH_TOKEN` — a long-lived token from `claude setup-token`, run once by the
  account the cloud jobs should bill to (a subscription), or
- `ANTHROPIC_API_KEY` — an API key from the Console (pay per use).

Set it in the server's environment like any other secret (`/etc/gator/server.env`, mode 600).
With Fly you may instead set it as an app secret (`fly secrets set -a gator-runners
CLAUDE_CODE_OAUTH_TOKEN=...`) and leave `GATOR_CLOUD_RUNNER_ENV` empty, so it never passes
through the server at all. Either way the agent sees it — it needs it to work — so use a
credential dedicated to runners, never a personal one.

**Git.** A machine starts empty and clones the project's repository over https. To push job
branches give it a token with write access to those repositories in `GITHUB_TOKEN` (named in
`GATOR_CLOUD_RUNNER_ENV`): the image's git credential helper hands it to the runner's own push.
The agent's environment does not carry `GITHUB_TOKEN`, so the agent cannot push by itself.
Without it, set `GATOR_RUNNER_PUSH=0` on the machines through the same mechanism and read the
results from the receipt.

## What an agent can reach

An agent runs as the runner's user with every permission bypassed, so whatever is in its
environment or readable by that user is the agent's to print and send. The runner's token must
be neither: it leases any job the runner may run.

- **Environment.** The agent gets an allow-list of the runner's environment — `PATH`, `HOME`,
  locale, the ssh agent, proxies, CA bundles and the backend's own login variables
  (`ANTHROPIC_*`, `CLAUDE_CODE_OAUTH_TOKEN`, `CLAUDE_CONFIG_DIR`) — plus the job's own `GATOR_URL`
  and `GATOR_JOB_TOKEN`. No `GATOR_*` variable of the runner reaches it, including
  `GATOR_RUNNER_TOKEN`. Name anything else it needs in `GATOR_RUNNER_AGENT_ENV`.
- **Token file on a Mac.** The agent runs under `sandbox-exec` with a profile that allows
  everything except opening the runner's token file (`GATOR_RUNNER_TOKEN_FILE`). The rest of the
  machine is as open to it as to the user — this protects the token, not the Mac.
  `GATOR_RUNNER_SANDBOX=0` turns it off for a backend that refuses to run inside a sandbox.
- **Token in the environment.** A runner given `GATOR_RUNNER_TOKEN` re-executes itself at
  start without it and takes the token over a pipe, because the environment a process starts
  with stays readable in `/proc/<pid>/environ` by every process of the same user. After that
  neither its environment nor the disk holds the token — this is how the runner image gets its
  one-job token. On Linux there is no file sandbox: a token *file* the runner user can read, the
  agent can read too, so on a VPS keep the token in `/etc/gator/runner.env` (root's, read by
  systemd) rather than in `GATOR_RUNNER_TOKEN_FILE`. What remains is the runner's memory, which
  another process can read only with ptrace rights over it; with the usual
  `kernel.yama.ptrace_scope=1` an agent, the runner's child, has none.
- **Keep the token file outside `GATOR_RUNNER_WORKDIR`**; the runner warns when it is not.

## Backups and restore

With `GATOR_BACKUP_S3_*` set the server uploads a `pg_dump` every hour and keeps the newest
`GATOR_BACKUP_KEEP`. `gator-server backup now` runs one immediately.
`gator-server restore <file>` replays a dump. The `restore-drill` workflow proves this weekly.
