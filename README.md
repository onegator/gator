# gator

Decision tool that maps the whole software delivery cycle, from idea to production monitoring.
Humans see only what depends on them; agents do the work; integrations are plugins.

This repository holds the Go side, four binaries from one module:

- `gator-server`: the brain. Owns process state, talks to runners and plugins, serves the API used by Gator.app.
- `gator-runner`: the worker. Executes jobs in git worktrees. The same binary runs on a VPS (systemd) and on a Mac (launchd).
- `gator-cli`: how an agent talks back while it works — blocked, a question, a document, a lesson. It travels with the runner and speaks with the job's own identity; see [docs/agent-tools.md](docs/agent-tools.md).
- `gator`: how a person works from a terminal — `gator login` with Google, projects, tasks, `gator task run --cloud`, `gator task logs -f`. Its token sits in the macOS keychain; see [docs/cli.md](docs/cli.md).

The only package shared by both is `internal/proto`. `internal/boundary_test.go` enforces that
`internal/server` and `internal/runner` never import each other.

Other repositories: `gator-app` (SwiftUI clients) and `gator-knowledge`. The plugins live here,
in `plugins/`.

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
`"project:<id>"` follows everything that happens in one project (a board uses it) and is
refused to anyone who may not read that project.

`/admin` is a minimal HTML panel for M1 and debugging; it goes away once Gator.app covers it.

## Conversations

People of a project talk in channels (`#general` comes with every project), DMs (inside a project
or outside every one) and threads under a message; every task has its thread, under its card in
the project's task channel (`process_config.conversations.task_channel`, `#general` by default).
Work shows up there as cards that follow their object: the task, the decision its gate waits for
(approved with the same `/tasks/{id}/approve`), its jobs and its documents. "Make this a task"
(`POST /messages/{id}/promote`) turns a message and its thread into a task. The rules live in
`internal/server/conversations`: `Access` decides who reads and writes (a viewer reads and reacts,
a private channel is its members', a DM its people's) for the API, the WebSocket and search
alike. Messages are numbered per conversation without gaps (`seq`); a repeated `clientId` answers
the first message. Live: `conversation:<id>` and `me` on the WebSocket; events carry ids and seq,
never message bodies. A mention, a DM or a reply in one's thread is pushed. Files people send (up
to 25 MB) are kept in `GATOR_ATTACHMENTS_S3_*` (endpoint, bucket, access key, secret key, prefix,
insecure) or `GATOR_ATTACHMENTS_DIR`; with neither set the server takes none.

## Agents

Agents are members of a project: `@researcher`, `@planner`, `@worker`, `@reviewer`, `@security`,
`@curator`, `@triage` and `@reporter` come with every project (the database seeds them), and a
project adds its own (`POST /projects/{id}/agents`, a `@backend-reviewer` for the reviewer role) or
sees the workspace's. A process template still says `role: reviewer`; the project's agent for the
role does the job, and every job, commit and card is signed by it. Mentioning an agent in a
conversation (or writing in a DM with one) is a turn: it answers in the thread under the message,
its answer streams into its message, its tool calls move the job's card, and what it cost shows
under it. A `chat` agent reads and answers with no tools that write in a read-only checkout; a
`work` agent changes code on a `gator/` branch only where a person allowed agents on the task, and
otherwise answers in chat and a card asks "Let @worker work on this?" (`POST /turns/{id}/allow`).
A conversation about no task opens a hidden task of kind `ask`; "make this a task" turns it into a
feature or a bug with its history. Agents delegate to each other with `gator-cli say` and an
`@handle`, within limits: four turns per chain, a chain budget of twice a turn of the asked agent,
never the same agent twice in a row, the project's daily budget, and a paused agent says so. A
person's reply while an agent answers steers it; Stop is `POST /turns/{id}/stop`. The rules live in
`internal/server/agents`, the conversation an agent is told in `internal/server/conversations/context.go`.

## Outside Gator: bridges and A2A

**Bridges** are plugins with the `messaging` capability: the bundled `slack` (channels, threads,
`/gator ask`, Approve on a decision card) and `github` (`@gator reviewer …` in a comment, the answer
as a comment). The core keeps their rules in `internal/server/bridges`: a bridge acts in its own
project, only on conversations it connected, never on a private one; what came through a bridge is
never mirrored back to it; an outside account is an identity of that project and bridge
(`external_identities`), heard by agents in chat and fenced as untrusted, approving nothing, until
its person links it with a code made in Gator (`POST /projects/{id}/external-links`, ten minutes,
once — never by email address); a linked member's Approve is `process.Approve` in their name. An
agent's answer streams out at most every 2 s.

**A2A**: a project's admin adds an agent outside Gator from its Agent Card (`POST
/projects/{id}/remote-agents`, card fetched through `netguard`, its bearer token encrypted with the
keyring). Mentioning it is a turn like any other, under the same hop, fan-out and budget guards; the
turn is an A2A task (`message/send`, then `tasks/get` until it ends) made by a worker in the server,
its answer written into the agent's message and carried to other agents as untrusted. It answers
only in conversations open to the whole project. Inbound, the admin exposes agents (`PUT
/agents/{id}/a2a`) and lets clients in (`POST /projects/{id}/a2a-clients`: a `gtr_a2a_…` token that
reaches only `/a2a/<project>/<agent>`, a daily budget, a rate). A client's ask is its message in
`#a2a-<client>`, answered in chat; it reads the answer from `tasks/get`. Cards are at
`/a2a/<project>/<agent>/.well-known/agent-card.json`.

**To check live before relying on it:** the A2A version and card path of the peers a team uses
(0.3, `/.well-known/agent-card.json`; older peers serve `agent.json`), whether they answer
`message/send` with a task or a message, and how they report cost (nothing reads it yet: a remote
turn costs $0 in Gator's records). Every test here talks to a fake peer.

## Brain

What the team and its agents know lives in one store, `brain_entries` (`internal/server/brain`):
scopes `workspace` (conventions everyone follows), `project` (vision, principles, decisions, runbooks),
`component` (handed to jobs about that component and its neighbours), `user` (a person's expertise,
preferences and focus: theirs alone unless shared) and `agent` (an agent's own lessons). Agents
propose, people approve; only approved entries reach a prompt; a change is a new version and the old
one stays (`GET /brain?at=` answers what held then). One `brain.Policy` decides who reads what for the
REST API (`/brain`), the MCP server and the package a job is handed: a job reads its project's approved
entries and the private context of the person its turn acts for (`agent_turns.on_behalf_of`), never of
anybody else. Every job is handed its part with `[brain:<id>]` tags; what it was handed, looked up and
cited is in `brain_citations`. The curator (`brain/curator`) proposes what a quiet task thread settled,
with one `brain_proposal` card a day in the thread; plugins propose with `brain.propose` (GitHub: the
ADRs in `docs/adr/`); a proposal that contradicts an approved decision asks whether it replaces it.
The old `/product` and `/knowledge` endpoints are facades over it for one release. The MCP server
`gator` is at `POST /mcp`: jobs reach it with their own token (the runner adds it to every session) and
people connect their own clients with theirs; see [docs/agent-tools.md](docs/agent-tools.md).

## Identity and access

- Browser login: `GET /auth/login` starts OIDC (`GATOR_OIDC_*`, e.g. Google) with PKCE and a
  nonce; the callback sets an httpOnly `gator_session` cookie. An account is created on first
  sign-in. `GATOR_ADMIN_EMAILS` names the workspace admins; unset, the first person in is admin.
  `GATOR_OIDC_ALLOWED_DOMAINS` limits who may create an account.
- Native login (Gator.app, CLI): `GET /auth/login?redirect_uri=…&state=…&code_challenge=…&code_challenge_method=S256`
  runs the same flow and sends the browser back to `gator://auth/…` or a loopback address with a
  one-time code; `POST /auth/token {code, code_verifier, redirect_uri, client_name}` trades it for
  a user token that lives 90 days. `GET /auth/config` says whether OIDC is on and the provider's name.
- Native clients and automation: `Authorization: Bearer gtr_<kind>_…`. Mint with `POST /api/v1/tokens`
  (user tokens for yourself; runner and plugin tokens require workspace admin). Only a SHA-256 hash is stored.
- Roles: workspace `admin` | `member`; per project `admin` | `member` | `viewer`. Any signed-in
  person may create a project and becomes its admin; a project admin manages its repositories and
  may archive it, while deleting an (empty) project stays with workspace admins. Each project in
  `GET /projects` carries the caller's `role`. Runners act as members
  everywhere but cannot approve; plugin tokens are members of their own project only.
- People: a project admin invites by email (`POST /projects/{id}/invitations`) and gets a link to
  pass on, `gator://invite/<token>` and its https form `/invite/<token>` (`GATOR_PUBLIC_URL` names
  the server); the server mails nothing yet. Signing in with the invited address joins the project,
  whatever `GATOR_OIDC_ALLOWED_DOMAINS` says, and so does `POST /invitations/accept` from someone
  already signed in with it. Links last 14 days and only their hash is stored. A project admin finds
  people already in the workspace with `GET /projects/{id}/candidates?q=`, changes roles and
  removes people; anyone may leave; the last admin can neither leave nor step down.
- `GATOR_PUBLIC_URL` is the address people's browsers use for this server (with Tailscale,
  `https://<host>.<tailnet>.ts.net`). It is the base of invitation links and of the OAuth callback
  plugin connections come back to (`GATOR_PUBLIC_URL/connect/callback`); unset, invitation links
  use the inviter's address and plugins cannot be connected.
- Every non-GET request is written to `audit_log` with actor, route pattern and status.
- Local development only: `GATOR_DEV_AUTH=1` trusts `X-Gator-User: <user uuid>`.

## Secrets, telemetry, limits

- Secrets at rest are AES-256-GCM under `GATOR_SECRETS_KEY` (`id:hex`). `gator-server secrets new-key` prints one;
  put the previous key in `GATOR_SECRETS_OLD_KEYS` while rotating. Logs pass through a redacting handler that masks
  any attribute whose key looks like a secret.
- OpenTelemetry traces and metrics export over OTLP when `OTEL_EXPORTER_OTLP_ENDPOINT` is set; otherwise no-op.
  Instruments: `gator.task.transitions`, `gator.task.phase_seconds`, `gator.gate.blocked`, `gator.jobs`,
  `gator.runners.online`, `gator.plugin.calls`, `gator.plugin.call_seconds`, `gator.agent.cost_usd`, `gator.http.rate_limited`.
- API rate limit per token (or IP when anonymous): `GATOR_RATE_LIMIT_RPS` / `GATOR_RATE_LIMIT_BURST`; 429 with `Retry-After`. The client IP comes from `X-Forwarded-For` only when the request arrives from `GATOR_TRUSTED_PROXIES` (default loopback).
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

1. `register` → `registered` with the negotiated protocol version (the server speaks N and N-1:
   v3 and v2). A v3 job says its kind (phase, reply, summary), tier (chat is read-only), agent and,
   for a reply, the conversation it answers in; a v2 runner is handed phase jobs only.
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
   Because the agent never stops to ask, no job starts on a task until a person has allowed
   agents on it, once per task (`POST /tasks/{id}/allow-agents`, or `allowAgents: true` on the
   first job). Until then the autopilot waits and the full inbox asks (`agents_not_allowed`).
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

### Agents speaking ACP

Any agent that speaks the [Agent Client Protocol](https://agentclientprotocol.com) runs as the
backend `acp:<name>`: `GATOR_ACP_AGENTS=codex:/usr/local/bin/codex-acp,gemini:/usr/local/bin/gemini
--experimental-acp` (name, colon, the command and its arguments). The runner starts the command in
the worktree for every run (`internal/runner/backend/acp`): `initialize`, `session/new` with Gator's
MCP servers over HTTP (when the agent can reach them; the job's token travels in the session's
headers, never on the command line), `session/prompt`, and `session/update` as the same job events
as Claude's. A steer is `session/cancel`, then `session/load` of the same session and the correction
(an agent that cannot load sessions starts over with the opening and the correction). The tool-call
limit and a cost the agent reports (`usage_update`) are counted by the runner, which cancels the
turn and ends the process group. The agent's permission requests are answered for the job's tier:
a work job allows them once each; a chat job allows reading, searching, fetching, the job's own MCP
servers and `gator-cli` alone, and refuses editing, deleting, moving and other commands, on a
read-only worktree — and runs only on an agent whose profile names a read-only session mode, which
the session must offer (not every tool of an agent asks first). A job with no tools (summaries)
never goes to an ACP backend. Each supported agent has a profile (its login variables
and files for `AuthState`, its read-only mode) and synthetic replays in `testdata/<agent>`: `codex`
(via codex-acp) and `gemini`; anything else runs as experimental. A policy picks it per role, tier or
agent (`"backend": "acp:codex"`).

**To check live before relying on it:** the adapters' current versions and flags (`codex-acp`,
`gemini --experimental-acp`), whether they report usage (`usage` in the prompt's answer,
`usage_update` with a cost), the ids of their session modes (codex-acp's `read-only`), and how they
title MCP tool calls in permission requests. The recordings here are synthetic: no model session
was started to make them.

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

## The component catalogue

A project lists what it is made of: components with a key, a kind (`app`, `api`, `lib`,
`infra`), the repository and path they live in, an owner, a line about what they do, and what
they depend on. `PUT /api/v1/projects/{id}/components` writes one; `PUT /api/v1/tasks/{id}/component`
says which part of the product a task is about.

A job whose task names a component gets that component, what it depends on, **what depends on
it** — the neighbours a change reaches — and the decisions linked to it from the project's
product context. The slice, not the catalogue: a worker fixing login gains nothing from the
shape of billing, and every extra document is budget spent on noise. A task with no component
gets nothing, so the catalogue costs a project nothing until it uses it.

Jobs record `context_bytes` and `context_docs` as they are leased, so whether the pack saves
tokens is a measurement rather than a claim.

## Digest and working state

Every job prompt asks the agent to end with a fenced `gator-digest` block of JSON: what it
changed, what it decided, what it rejected and what is left. The runner removes the block from
the answer people read and puts it in the receipt; a done session that forgot it is resumed
once, in the same session, to ask for only the block. Each done job then rebuilds the task's
`working_state` artifact from all receipts: work so far, decisions, rejected approaches, what is
left, and the latest branch and commit. The next job of the task gets it as its first context
document whatever backend or model runs it. `job.digest` events carry each digest for the
curator (M5); `job.digest_missing` records sessions that gave none.

## Backend, model and budget policy

`process_config.policy` picks the backend, model and per-job cost cap for each role, and a
daily budget for the project:

```json
{"policy": {
  "default": {"backend": "claude", "model": "claude-opus-5", "max_cost_usd": 5},
  "roles": {"reviewer": {"model": "claude-sonnet-5", "max_cost_usd": 1}},
  "daily_budget_usd": 25,
  "placement": "local"
}}
```

`placement` is where the autopilot's jobs run, `local` (the default) or `cloud`; a person starting
a job chooses for that job. See [Where a job runs](docs/deploy.md#where-a-job-runs).

A role's entry overrides the default; a role that switches backend drops the default model.
Jobs created without a backend or model take them from the policy. The cost cap travels in the
job bounds: Claude gets `--max-budget-usd`, and the runner fails any job whose receipt went over
it. Runners only lease jobs for backends they report as logged in; a job waits rather than
running on a different backend, since there is no silent substitution. Once the project has
spent its daily budget (UTC day, from usage records), new jobs are refused with 409 and the
autopilot fails the gate's `budget` check, which blocks the task in the inbox; the next
reconcile after midnight or a higher limit passes the check and the autopilot resumes.

## Plugins

Integrations are plugins: separate processes speaking JSON-RPC 2.0 over stdio, one process
per project and plugin, started only by gator-server. The ones it ships (`GATOR_PLUGIN_DIR`)
are listed by `GET /api/v1/plugins/catalogue` and switched on per project with
`PUT /api/v1/projects/{id}/plugins/{name}`; GitHub and Linear connect through OAuth instead of a
pasted token (callback: `GATOR_PUBLIC_URL/connect/callback`). Others are installed with `PUT /api/v1/plugins/{name}`; webhooks arrive at
`POST /hooks/<project-slug>/<plugin>`. The contract and a Go SDK live in the public package
`plugin/`; the protocol is in [docs/plugins.md](docs/plugins.md).

## Marketplace

Agents, skills, remote MCP servers, native plugins, processes and knowledge packs come from
registries (`internal/server/marketplace`): git repositories (or directories) whose `index.json` names
every package with its version and the sha256 of its files, signed with Ed25519 in `index.json.sig`.
Gator's own is built in (`GATOR_MARKETPLACE_URL`, default
[onegator/gator-marketplace](https://github.com/onegator/gator-marketplace), verified against
`GATOR_MARKETPLACE_PUBLIC_KEY`) and fetched at start; a workspace admin adds the company's own
(`POST /api/v1/registries`). A bad signature takes nothing; a package that does not match its
checksum, carries a link or an unsafe path, breaks the size limits, or is a published version whose
files changed, is turned away. Installing (`POST /api/v1/projects/{id}/installations`, or
`/workspace/installations`) needs every permission the version asks for accepted; an agent is in the
project's agents at once. Nothing updates by itself: a newer version is offered
(`GET /installations/{id}/update`) and applied by a person (`PATCH /installations/{id}`), accepting
what it adds and saying what becomes of what they changed. `gator-server marketplace
checksum|index|keygen|sign|verify` is what a registry's maintainer runs.

A job's lease carries its agent's skills (Claude Code loads them with `--plugin-dir` from the
runner's own directory; other backends read them through `skill_read`) and names the remote MCP
servers it was given. The runner reaches each through Gator's proxy, `POST /mcp/proxy/<name>`, with the
job's own token: the server's credentials never leave gator-server — a token, or OAuth the way the
MCP spec has it (the server's metadata, its authorization server's, a client registered for it alone,
PKCE), connected through `/connect/start` and `/connect/callback` in the browser that started it.
Every call is written to `mcp_calls` (arguments masked), tools outside the allowlist (and, for chat,
outside the read-only tools allowed for chat) are refused, answers come back bounded and fenced as
untrusted, and a server that keeps failing is not called for a while. The server's own requests reach
no private address (`netguard`) unless `GATOR_OUTBOUND_ALLOW_PRIVATE=1`.

## License

[MIT](LICENSE).
