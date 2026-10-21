# github

Gator's reference plugin for GitHub. It uses the REST API, with the token a person gets by
pressing **Connect GitHub** in the project's settings (an OAuth app's token with the `repo`
scope), or with a fine-grained token pasted instead. A GitHub App, with per-repository
installation tokens and one app-wide webhook, comes later.

## What it does

| On | It does |
|---|---|
| switched on, connected, or **Import issues** | brings the repository's open issues in as tasks, newest first and at most 500 at a time; issues already on the board are counted, not repeated |
| `issues` opened, reopened or labeled | creates a task once per issue: kind `bug` when the issue has a `bug` label, else `feature` (starting in Idea). With `trigger_label` set, only labelled issues count. A reopened issue reopens its task. |
| `issues` edited | the task takes the issue's title and body |
| `issues` closed | closes the task, unless the task has Gator's pull request: that one says "Closes #n", and the task finishes by its own process (a release may still be ahead) |
| a task closes | closes its issue as completed, if it is still open |
| the plugin starts | registers the repository webhook (Issues, Pull requests, Check runs), or rewrites the one it made, when the server knows its public address (`GATOR_HOOKS_URL` or `GATOR_PUBLIC_URL`) |
| a worker job finishes with commits | opens a pull request from the job's branch with the job's report, `Closes #<issue>` and a `gator:<phase>` label. A retry refreshes the existing pull request. |
| `pull_request` events | records the pull request on the task (artifact, state for the chip) |
| `check_run` events | sets `github/<check>` on the gate while the task is in `check_phase` (default Implementation); a failing check blocks the gate, a passing one unblocks it |
| a task enters `check_phase` | reports the pull request's current check runs |
| a phase change | moves the `gator:<phase>` label on the issue and the pull request |
| a task opens in a client | a GitHub tab with links, and chips for the issue and the pull request |

Deliveries for other repositories are logged and ignored. A bad signature is refused with
400. GitHub does not retry a 400, and a refusal does not count toward disabling the plugin.

## Settings

| Setting | |
|---|---|
| `repo` | `owner/name` |
| `token` (secret) | set by **Connect GitHub**. Or a fine-grained token for this repository. Permissions: **Contents**, **Issues**, **Pull requests** and **Webhooks** read and write; **Checks** and **Metadata** read. Without Checks the plugin cannot read CI results: GitHub answers 403, the plugin is disabled with that reason, and a gate waiting on a check never learns it finished. |
| `webhook_secret` (secret) | set by **Connect GitHub**; or the secret on a webhook made by hand |
| `api_url` | default `https://api.github.com` |
| `base_branch` | default: the repository's default branch |
| `trigger_label` | only issues with this label become tasks |
| `label_prefix` | default `gator:` |
| `check_phase` | default `implementation` |

## Webhook

The plugin makes the webhook itself when the server knows its public address. Without one,
or with a token that may not manage webhooks, add it in the repository settings:

- **Payload URL:** `https://<gator>/hooks/<project-slug>/github`
- **Content type:** `application/json`
- **Secret:** the same value as `webhook_secret`
- **Events:** Issues, Pull requests and Check runs

## Limits

- **Branch pushes:** the runner pushes job branches to the repository. That needs push access
  on the runner's side (a deploy key or token), which is separate from this plugin's token.
- **Forks:** pull requests from forks are not handled.
- **Payloads:** `testdata/payloads` holds webhook payloads trimmed to the fields the plugin
  reads, shaped after GitHub's documented schema. Deliveries recorded from a real repository
  replace them once the public webhook endpoint exists.
