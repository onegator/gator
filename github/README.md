# github

Gator's reference plugin for GitHub. It uses the REST API with a fine-grained personal
access token. A GitHub App, with per-repository installation tokens, comes later.

## What it does

| On | It does |
|---|---|
| `issues` opened, reopened or labeled | creates a task once per issue: kind `bug` when the issue has a `bug` label, else `feature` (starting in Idea). With `trigger_label` set, only labelled issues count. |
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
| `token` (secret) | fine-grained token for this repository. Permissions: Issues and Pull requests read and write; Contents and Metadata read; Checks read where offered. |
| `webhook_secret` (secret) | the secret on the repository webhook |
| `api_url` | default `https://api.github.com` |
| `base_branch` | default: the repository's default branch |
| `trigger_label` | only issues with this label become tasks |
| `label_prefix` | default `gator:` |
| `check_phase` | default `implementation` |

## Webhook

In the repository settings, add a webhook:

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
