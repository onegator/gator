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
Role prompts live in `process/roles/`.

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
