# gator

Decision tool that maps the whole software delivery cycle, from idea to production monitoring.
Humans see only what depends on them; agents do the work; integrations are plugins.

This repository holds the Go side, two binaries from one module:

- `gator-server`: the brain. Owns process state, talks to runners and plugins, serves the API used by Gator.app.
- `gator-runner`: the worker. Executes jobs in git worktrees. The same binary runs on a VPS (systemd) and on a Mac (launchd).

The only package shared by both is `internal/proto`. `internal/boundary_test.go` enforces that
`internal/server` and `internal/runner` never import each other.

Other repositories: `gator-app` (SwiftUI clients), `gator-plugin-github`, `gator-knowledge`.

## Develop

```bash
mise install            # Go version pinned in mise.toml
make db-up              # PostgreSQL 16 in Docker
cp .env.example .env    # then export or use direnv
make migrate
make run-server         # http://localhost:8080/healthz
```

Migrations run only when asked (`gator-server migrate`), never at startup. Every migration has a down step.

## Process templates

Phase templates per task kind live in `internal/server/process/defaults/*.yaml` and are embedded
in `gator-server`. A project overrides a kind by placing a full template under `process_config`.
Phases with `requires: deploy` are active only when the project has a plugin with that capability.
Role prompts (researcher, planner, worker, reviewer) live in `internal/server/process/roles/*.md`
and are embedded too; a project replaces one with `process_config.roles.<role>`.

## API

`docs/openapi.yaml` is the contract. `make generate` regenerates the chi server and models
(`internal/server/api/gen`) and the sqlc queries. Live updates: WebSocket at `/api/v1/ws`,
first message `{"subscribe":["inbox","task:<id>"]}`; every domain event is written to the
`events` outbox in the same transaction as the change and relayed to subscribers.

`/admin` is a minimal HTML panel for M1 and debugging; it goes away once Gator.app covers it.

## Identity and access

- Browser login: `GET /auth/login` starts OIDC (`GATOR_OIDC_*`); the callback sets an httpOnly
  `gator_session` cookie. The first user to log in becomes workspace admin.
- Native clients and automation: `Authorization: Bearer gtr_<kind>_…`. Mint with `POST /api/v1/tokens`
  (user tokens for yourself; runner and plugin tokens require workspace admin). Only a SHA-256 hash is stored.
- Roles: workspace `admin` | `member`; per project `admin` | `member` | `viewer`. Runners act as members
  everywhere but cannot approve; plugin tokens are members of their own project only.
- Every non-GET request is written to `audit_log` with actor, route pattern and status.
- Local development only: `GATOR_DEV_AUTH=1` trusts `X-Gator-User: <user uuid>`.

## Secrets, telemetry, limits

- Secrets at rest are AES-256-GCM under `GATOR_SECRETS_KEY` (`id:hex`). `gator-server secrets new-key` prints one;
  put the previous key in `GATOR_SECRETS_OLD_KEYS` while rotating. Logs pass through a redacting handler that masks
  any attribute whose key looks like a secret.
- OpenTelemetry traces and metrics export over OTLP when `OTEL_EXPORTER_OTLP_ENDPOINT` is set; otherwise no-op.
  Instruments: `gator.task.transitions`, `gator.task.phase_seconds`, `gator.gate.blocked`, `gator.jobs`,
  `gator.runners.online`, `gator.plugin.calls`, `gator.plugin.call_seconds`, `gator.agent.cost_usd`, `gator.http.rate_limited`.
- API rate limit per token (or IP when anonymous): `GATOR_RATE_LIMIT_RPS` / `GATOR_RATE_LIMIT_BURST`; 429 with `Retry-After`.
- `internal/server/limits.Breaker` is the circuit breaker the plugin host wraps every plugin with (M3).

## Background jobs and backups

River (Postgres-backed queue) runs periodic work: a phase-timeout sweep every minute that blocks
overdue tasks into the inbox, and an hourly `pg_dump` uploaded to an S3-compatible bucket when
`GATOR_BACKUP_S3_*` is set (`GATOR_BACKUP_KEEP` prunes old dumps). `gator-server backup now` runs one
immediately; `gator-server restore <file>` replays a dump with `pg_restore --clean`. River's own
schema is applied by `gator-server migrate`. On SIGTERM the server stops HTTP, lets in-flight jobs
finish for up to 15s, then forces the queue down.

## Time and tokens per task

Every task is measured. Time comes from the append-only transition log: lead time (created to
closed, or to now), wall time per phase and visit count (rollbacks add visits), and blocked time.
Tokens, cost and agent run time come from `usage_records`, one row per job or report, split into
input, output, cache read and cache write. Runners attach a `proto.Usage` to every `Finish`;
anything else can report through `POST /api/v1/tasks/{id}/usage` with an idempotency key.
Cost on subscriptions is an estimate from list prices and is flagged `costEstimated`.

- `GET /api/v1/tasks/{id}/metrics`: totals and per-phase breakdown.
- `GET /api/v1/projects/{id}/metrics?since=`: per task kind, with averages over closed tasks.
- OpenTelemetry counters `gator.agent.tokens{type,backend}` and `gator.agent.cost_usd{backend}`.

## Deploy

`docs/deploy.md` covers a VPS with systemd (`deploy/install.sh`), Docker (`deploy/compose.yaml`)
and the Mac runner (launchd). Tagging `v*` publishes signed-checksum archives for both binaries
and `ghcr.io/onegator/gator-server` for amd64 and arm64.

## Runner protocol

Runners hold one WebSocket to `/api/v1/runner`, authenticated with a runner token in the
handshake. The contract lives in `internal/proto` and is the only code both binaries share.

1. `register` → `registered` with the negotiated protocol version (the server speaks N and N-1).
2. `heartbeat` every 15 s lists the jobs the runner holds; only those leases are extended.
   45 s of silence marks the runner offline.
3. `lease_request` → `lease`: jobs are handed out with `FOR UPDATE SKIP LOCKED`, matched on
   backend and optional project list, capped by the runner's free slots.
4. `events` stream agent output; `(job, seq)` makes a resend after reconnect a no-op.
5. `finish` carries the receipt and usage. `done` without usage is recorded as `failed`.
   Usage lands in the task's metrics with the job id as idempotency key.
6. `ack` confirms events and finishes; the runner keeps them until acked and resends after
   reconnecting. A job stays in the heartbeat until its finish is acked.

The server sweeps every 10 s: a lease nobody extended for 90 s returns the job to the queue
(or fails it after `maxAttempts`), and a running job with no output for 10 minutes becomes
`stalled` until its next event. People steer and stop jobs through `/api/v1/jobs/{id}/steer`
and `/stop`; live output streams on the WebSocket topic `job:<id>`.

## What a runner does with a job

1. **Workspace.** Each repository is cached once as a bare clone under `$WORKDIR/repos`; the job
   gets its own worktree on branch `gator/<task>-<job>` from `origin/<default branch>`. Jobs of
   projects without a repository get an empty directory.
2. **Agent.** The backend runs `claude -p --output-format stream-json --verbose --permission-mode
   bypassPermissions --strict-mcp-config --mcp-config <explicit or empty>` in its own process group,
   so MCP servers and claude.ai connectors of the logged-in account never reach a job. Output becomes job events (`session`, `text`,
   `tool_call`, `tool_result`, `rate_limit`, `result`); hook output and thinking are not forwarded.
3. **Bounds.** The job timeout and tool-call limit end the whole process group; a process that
   exits without a `result` line is `failed`, never quietly `done`.
4. **Steer.** A person's correction interrupts the current run and resumes the same session
   with `--resume`, so context is kept. Usage adds up across runs.
5. **Git.** Commits on the job branch are counted and the branch alone is pushed to origin;
   the worktree is removed. An unpushed branch stays in the cache.
6. **Receipt.** The finish carries status, branch, commits, changed files, summary, session id
   and usage summed over every model the session used. Unacked events and receipts survive a
   runner restart in `$WORKDIR/outbox.json`.

`internal/runner/backend/claude/testdata` holds sanitized recordings of real Claude Code 2.1.270
sessions; the parser and backend tests replay them through a fake `claude`.

## Autopilot, roles and artifacts

When a task enters a phase owned by a runner (discovery, planning, implementation, verification
in the default templates), the server queues one job for it: the phase's role, the project's
backend (`process_config.autopilot.backend`, default `GATOR_DEFAULT_BACKEND` or `claude`). Task
events trigger it within moments; a reconcile every 10 s catches anything missed. A job is
created once per phase entry, so a rollback gets a fresh one, and a failed job is not retried
automatically: it shows in the inbox as `job_failed` for a person to decide. Turn it off per
project with `{"autopilot":{"enabled":false}}` or for the server with `GATOR_AUTOPILOT=0`.

The lease carries the role's prompt, the task's title and description, the job's instruction,
the latest version of every artifact so far, and the reason the task was last sent back to this
phase. A job that ends `done` leaves the document its role produces (brief, plan, report,
review) as a new artifact version; approving the phase approves its artifacts. While a job is
queued or running, the task does not ask anyone for a decision.
