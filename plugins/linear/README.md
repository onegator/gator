# linear

Gator's tracker plugin for [Linear](https://linear.app). An issue becomes a task, the task's
phase is mirrored in the issue's workflow state, and the task detail links back to it.

Gator owns the phase; Linear mirrors it. People who live in Linear see where the work is
without opening Gator, and nobody has to move a card by hand.

## What it does

| On | It does |
|---|---|
| an issue created | creates a task once per issue: kind `bug` when it has a `Bug` label, else `feature`. With `trigger_label` set, only labelled issues count — and adding the label to an existing issue brings it in |
| a phase change | moves the issue to the state `state_map` names for the new phase; phases not listed leave it where it is |
| a task opens in a client | a Linear tab with the link and a chip with the issue id |

Deliveries for another team (with `team` set) are logged and ignored. A bad signature is
refused with 403.

## Settings

| Setting | |
|---|---|
| `webhook_secret` (secret) | the signing secret of the Linear webhook |
| `api_key` (secret) | personal API key, needed to move issues. Without it issues still become tasks, but Linear never hears what happened to them |
| `team` | team key, like `ENG` |
| `trigger_label` | only issues with this label become tasks |
| `state_map` | `implementation=In Progress, verification=In Review, approved=Done` — state names as your team calls them |
| `api_url` | default `https://api.linear.app/graphql` |

## Webhook

In Linear: Settings → API → Webhooks, point one at

```
https://<gator>/hooks/<project-slug>/linear
```

with the **Issues** resource, and copy its signing secret into `webhook_secret`.
