# linear

Gator's tracker plugin for [Linear](https://linear.app). An issue becomes a task, the task's
phase is mirrored in the issue's workflow state, an issue finished or reopened in Linear is
finished or reopened in Gator, and the task detail links back to it. **Connect Linear** in the
project's settings is all it needs; a personal API key and a webhook made by hand still work.

Gator owns the phase; Linear mirrors it. People who live in Linear see where the work is
without opening Gator, and nobody has to move a card by hand.

## What it does

| On | It does |
|---|---|
| switched on, connected, or **Import issues** | brings in the issues that are not finished (of `team`, with `trigger_label`, when set), newest first and at most 500 at a time; issues already on the board are counted, not repeated |
| an issue created | creates a task once per issue: kind `bug` when it has a `Bug` label, else `feature`. With `trigger_label` set, only labelled issues count — and adding the label to an existing issue brings it in |
| an issue updated | the task takes its title and description. Moved to a completed or canceled state, the task closes; moved back from one, it reopens. Other states leave the phase alone: which phase the work is in is Gator's to say, and the state Gator itself set for the phase is recognised as its own echo |
| a phase change | moves the issue to the state `state_map` names for the new phase; phases not listed leave it where it is |
| a task closes | moves its issue to the state `state_map` names for `closed`, or the team's first completed state, unless it is finished already |
| the plugin starts | registers the Linear webhook (Issues), or rewrites the one it made, when the server knows its public address and the token may (Connect Linear asks for `admin`) |
| a task opens in a client | a Linear tab with the link and a chip with the issue id |

Deliveries for another team (with `team` set) are logged and ignored. A bad signature is
refused with 403.

## Settings

| Setting | |
|---|---|
| `api_key` (secret) | set by **Connect Linear**; or a personal API key. Needed to import and to move issues. Without it issues still become tasks from the webhook, but Linear never hears what happened to them |
| `webhook_secret` (secret) | set by **Connect Linear**; or the signing secret of a webhook made by hand |
| `team` | team key, like `ENG` |
| `trigger_label` | only issues with this label become tasks |
| `state_map` | `implementation=In Progress, verification=In Review, closed=Done` — state names as your team calls them; Gator.app edits it as rows |
| `api_url` | default `https://api.linear.app/graphql` |

## Webhook

The plugin makes the webhook itself when connected by a Linear admin and the server knows its
public address. Otherwise, in Linear: Settings → API → Webhooks, point one at

```
https://<gator>/hooks/<project-slug>/linear
```

with the **Issues** resource, and copy its signing secret into `webhook_secret`.
