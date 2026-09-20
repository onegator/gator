# Plugins

Gator's core knows no external system. GitHub, trackers, CI, monitoring and deploys are
plugins: separate processes that talk to `gator-server` over JSON-RPC 2.0 on stdin and stdout.
Any language works. Go plugins can use the SDK in `github.com/onegator/gator/plugin`.

Runners never start plugins and never see plugin secrets. Only `gator-server` does.

## Process model

- The server starts **one process per (project, plugin)**. A project's secrets live only in the
  environment of its own process.
- The environment is minimal: `PATH`, `HOME`, `TMPDIR`, `LANG`, `GATOR_PLUGIN_PROJECT`, and one
  `GATOR_SECRET_<NAME>` per secret setting (the name is uppercased, and any other character
  becomes `_`). The server's own environment, such as the database URL or the keyring, never
  reaches a plugin.
- stdout carries protocol messages only. Write logs to stderr; the server logs every line,
  tagged with the plugin and the project.
- A process that exits is restarted with backoff (1 s doubling to 1 min).
- On shutdown the server sends the `shutdown` notification, closes stdin, and kills the process
  group after 3 s.

## Wire format

One JSON-RPC 2.0 message per line (newline-delimited JSON, at most 8 MiB per message). Both
sides send requests: the core calls hooks, the plugin calls core methods, and each side answers
the other's requests. Serve requests concurrently: a hook may call back into the core before it
answers.

```
→ {"jsonrpc":"2.0","id":1,"method":"initialize","params":{"core_version":"1.0.0","protocol_version":1,"project_id":"…","project_slug":"app","config":{"repo":"acme/app"}}}
← {"jsonrpc":"2.0","id":1,"result":{"name":"github","version":"0.1.0","capabilities":["vcs","ci"],"hooks":["webhook","jobFinish"],…}}
```

Errors use JSON-RPC codes plus Gator's own:

| Code | Meaning |
|---|---|
| -32601 | method not found or hook not handled |
| -32602 | invalid params; use it to reject a bad webhook |
| -32603 | internal error; counts toward disabling the plugin |
| -32001 | exists in the protocol but not in this core yet (`incident.upsert`, `release.record` until M6) |
| -32003 | forbidden; use it for a bad webhook signature |
| -32004 | not found, including tasks of other projects |

## Manifest

`initialize` returns the manifest. At install time the server calls it without a project,
config or secrets, just to read the manifest.

```json
{
  "name": "github",
  "version": "0.1.0",
  "min_core_version": "1.0.0",
  "capabilities": ["vcs", "ci"],
  "hooks": ["webhook", "phaseTransition", "gateEvaluate", "jobFinish", "renderUI"],
  "config_schema": {
    "type": "object",
    "properties": {
      "repo": {"type": "string", "description": "owner/name"},
      "token": {"type": "string", "x-secret": true},
      "webhook_secret": {"type": "string", "x-secret": true}
    },
    "required": ["repo", "token"]
  },
  "ui": ["tab", "chip"],
  "schedule": [{"name": "sync", "every": "10m"}],
  "webhook": {"delivery_header": "X-GitHub-Delivery"}
}
```

- **capabilities**: `tracker`, `vcs`, `ci`, `monitoring`, `deploy` and `notify`. The core asks
  for capabilities, never for plugin names. A project with a `deploy` plugin gets the Release
  and Monitoring phases.
- **config_schema**: a subset of JSON Schema: one object whose properties are `string`,
  `number`, `integer`, `boolean`, `array` or `object`, plus `required` and `enum`. A property
  with `"x-secret": true` (or `"writeOnly": true`) is a secret string. The server encrypts it,
  never returns or logs it, and passes it only as an environment variable.
- **min_core_version**: installing on an older core fails. Development and snapshot builds of
  the core accept any value.

## Hooks (core → plugin)

| Method | When | Params → result |
|---|---|---|
| `initialize` | process start | `InitializeParams` → manifest |
| `shutdown` | notification before stop | none |
| `webhook` | `POST /hooks/<project-slug>/<plugin>` | `{delivery_id, headers, body}` → null |
| `phaseTransition` | after every advance or rollback | `{task, from, to, rollback, reason}` → null |
| `gateEvaluate` | a task is created, enters a phase, or its gate has sat on a pending check | `{task, phase}` → `{checks: [{name, status, detail}]}` |
| `artifactApproved` | a person approved a phase's documents | `{task, phase}` → null |
| `schedule` | every `every` of a manifest schedule | `{name}` → null |
| `renderUI` | a client opens a task | `{task}` → `{tabs: [{title, markdown}], chips: [{text, color, url}]}` |
| `jobPrepare` | before a job is leased to a runner | `{task, job}` → `{instructions}` (runners see it: no secrets) |
| `jobFinish` | after a job's receipt | `{task, job, receipt}` → null |
| `release`, `incidentClosed` | M6 | none yet |

Every call has a time limit (30 s by default). Hooks are delivered **at least once**: the
server reads domain events from its outbox with a durable cursor, so a restart can repeat a
batch. Make hooks idempotent, for example by looking up an existing task with `task.find`
before creating one. Checks from `gateEvaluate` are stored as `plugin:<name>` checks, and a
`fail` blocks the gate.

A webhook, by contrast, arrives **at most once**: senders like GitHub do not retry a delivery
their side dropped, and three in a row were lost here to a proxy answering 502. A gate then
keeps showing the last thing it heard. So every 5 minutes the server re-runs `gateEvaluate` for
gates whose plugin checks have sat at `pending` for 15 minutes. Answer it from the source of
truth rather than from what the plugin remembers, and make it cheap: it is called on a timer.

## Core methods (plugin → core)

All of them are scoped to the plugin's project. Tasks of other projects look as if they do
not exist.

| Method | Params → result |
|---|---|
| `task.create` | `{kind, title, description, urgency, external_refs}` → task |
| `task.find` | `{key, value}` → the newest task with `external_refs[key] == value`, or null |
| `task.update` | `{task_id, external_refs}` → task (refs are merged) |
| `gate.setCheck` | `{task_id, name, status, detail}` → null |
| `artifact.put` | `{task_id, phase?, type, content? , url?}` → `{version}` |
| `kv.get` / `kv.put` | `{key}` → `{found, value}` / `{key, value}` → null (JSON up to 64 KiB, per project) |
| `log` | `{level, message, fields}` → null |
| `incident.upsert`, `release.record` | answer -32001 until M6 |

## Webhooks

`POST /hooks/<project-slug>/<plugin>` is the only endpoint that needs no Gator token. The
plugin verifies the sender's signature itself, using a secret setting. The core also applies
these protections:

- **Body size:** at most 1 MiB, otherwise 413.
- **Rate limit:** 10 requests per second, burst 20, for each project and plugin; otherwise 429.
- **One delivery, one run:** the delivery id comes from the manifest's `delivery_header`, else
  from `X-Gator-Delivery`, else from the body's SHA-256. A delivery id that was already handled
  gets 200 `{"status":"duplicate"}` without calling the plugin.
- **Headers:** `Authorization`, `Cookie` and `Proxy-Authorization` are never forwarded.
- **Answers:** the answer tells the sender whether to retry. A failed call forgets the
  delivery id, so the sender's retry is processed.

| Outcome | Status | Sender retries? |
|---|---|---|
| plugin accepted the webhook | 200 | no |
| plugin refused it (-32602 or -32003) | 400 | no |
| plugin failed | 502 | yes |
| plugin not running | 503 | yes |
| call timed out | 504 | yes |

## Failures and audit

Every call in either direction is stored in `plugin_calls`, which is append-only. Secrets are
removed by field name and by value; payloads over 64 KiB are summarised. Project admins read
the log with `GET /api/v1/projects/{id}/plugins/{name}/calls`.

After 5 failures in a row the plugin is disabled for that project, and the reason is shown in
`GET /projects/{id}/plugins`. Only these count as failures: a timeout, a crash, no process, or
-32603. A plugin's deliberate refusals (-32602, -32003) do not count, so outsiders cannot
switch a plugin off by sending bad webhooks. Configuring the plugin again re-enables it.

## Managing plugins

- `PUT /api/v1/plugins/{name}` with body `{"command": ["/path/to/plugin", "--flag"]}` installs a
  plugin or updates it. Workspace admins only. The command runs as the server's user.
- `PUT /api/v1/projects/{id}/plugins/{name}` with body `{"enabled": true, "config": {...}}`
  enables, disables or configures a plugin for a project. Project admins only.
  - A secret setting left out keeps its stored value.
  - An empty string removes the secret.
  - The response lists which secrets are set, never their values.
- `GET /api/v1/tasks/{id}/plugin-ui` returns the tabs and chips for a task.

## Writing a plugin in Go

```go
package main

import (
	"context"

	"github.com/onegator/gator/plugin"
)

func main() {
	_ = plugin.Serve(plugin.Handlers{
		Manifest: plugin.Manifest{Name: "hello", Version: "0.1.0", Capabilities: []string{"notify"}},
		PhaseTransition: func(ctx context.Context, core *plugin.Core, p plugin.PhaseTransitionParams) error {
			return core.Log(ctx, "info", p.Task.Title+" moved to "+p.To, nil)
		},
	})
}
```

`Serve` fills `hooks` from the handlers you set. `core.Setting(key)` and `core.Secret(key)`
read the configuration, and the `core` methods map one-to-one to the table above.
`internal/server/plugins/testdata/echo` is a complete example that exercises every hook.

## Testing without a server

`gator-plugin` runs a plugin against an in-memory core that follows the server's rules: task
kinds and first phases, per-project scoping, check and artifact versioning, and the key-value
store. It is built from `cmd/gator-plugin`:

```sh
go run github.com/onegator/gator/cmd/gator-plugin new acme-tracker plugins/acme-tracker
cd plugins/acme-tracker && go build -o bin/acme-tracker .
go run github.com/onegator/gator/cmd/gator-plugin dev scenarios/basic.json -- ./bin/acme-tracker
go run github.com/onegator/gator/cmd/gator-plugin manifest -- ./bin/acme-tracker
```

- `new` writes `main.go`, `scenarios/basic.json` and a README. The skeleton plugin verifies a
  signed webhook, creates a task once per event, and logs phase changes. It never overwrites
  files.
- `dev` plays a scenario and prints each step, the core calls the plugin made, and the final
  state. It exits 1 when a step misses its expectation. Use `-json` for a machine-readable
  report.

A scenario seeds a project and calls hooks in order:

```json
{
  "project": {"slug": "demo"},
  "config": {"repo": "acme/app"},
  "secrets": {"webhook_secret": "dev-secret"},
  "tasks": [{"id": "t1", "kind": "feature", "title": "Search", "phase": "planning"}],
  "steps": [
    {"hook": "webhook", "body_file": "payloads/issue-opened.json",
     "headers": {"X-GitHub-Event": "issues"},
     "sign": {"header": "X-Hub-Signature-256", "secret": "webhook_secret", "prefix": "sha256="},
     "expect_core": ["task.create"]},
    {"hook": "phaseTransition", "task": "t1", "from": "planning", "to": "implementation"},
    {"hook": "gateEvaluate", "task": "t1"},
    {"hook": "jobFinish", "task": "t1", "receipt": {"status": "done", "branch": "gator/t1"}},
    {"hook": "webhook", "body": {"forged": true}, "expect_error": true}
  ]
}
```

- **Webhooks:** `sign` adds an HMAC-SHA256 of the body, keyed with a secret setting. Webhook
  bodies can be recorded payloads (`body_file`, resolved next to the scenario) or inline JSON.
- **Tasks:** steps name tasks by id, either seeded ones or the id of a task a plugin created.
- **Expectations:** `expect_error` expects the hook to fail, and `expect_core` lists core
  methods the plugin must call in that step.

Go plugins can use the same machinery in their own tests: `plugintest.NewCore`,
`plugintest.Start` and `plugintest.Run` in `github.com/onegator/gator/plugin/plugintest`.

`onegator/gator` is a public module, so a plugin in its own repository fetches the SDK with
nothing special: no GOPRIVATE, no token, locally or in CI.
