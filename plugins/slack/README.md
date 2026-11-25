# slack

Gator's bridge to [Slack](https://slack.com): the team keeps talking where it is, and the
conversation still lives in Gator — its Brain, cards and metrics see all of it. The plugin has the
`messaging` capability; the rules it is held to are the core's (`internal/server/bridges`).

## What it does

| In Slack | In Gator |
|---|---|
| a message in a connected channel | a message in the project channel it maps to, by its author |
| a reply in a thread | a reply in the thread under the same message |
| `@Gator reviewer look at #12`, `@reviewer …` | the agent is asked; its answer streams into the Slack thread (edited every ~2 s) |
| `/gator ask @reviewer …` | the question is posted to the channel by the bot and asked, the answer comes in its thread |
| `/gator link GTR-…` | the Slack account is linked to the Gator person who made the code |
| **Approve** on a decision card | the task is approved, for a linked account whose person is a member of the project |
| an edit of a message | the same message changes in Gator |

What is said in Gator in a connected channel (people, agents, cards) is mirrored to Slack. Words
are escaped, so `<!channel>` written by anyone pings nobody, and links are not unfurled. What came
from Slack is never sent back to it.

A Slack user without a linked account is an outside identity of this project: agents hear them,
answer in chat, and read their words as untrusted data; they approve nothing and let no agent work.
The same Slack user in another project is somebody else there. Linking needs a code the person made
in Gator (`POST /api/v1/projects/{id}/external-links` with `{"provider":"slack"}`): it works once,
for ten minutes, for this project. An email address never links an account.

## Settings

| Setting | |
|---|---|
| `bot_token` (secret) | set by **Connect Slack** (`xoxb-…`) |
| `signing_secret` (secret) | the app's signing secret (Basic Information). Every request is checked against it and refused when older than five minutes |
| `channels` | Slack channel ids and the project channels they are: `C0123ABC=eng,C0456DEF=ops`. Only these are bridged. A private Gator channel never is |
| `api_url` | default `https://slack.com/api`; another host only if the operator lists it in `GATOR_PLUGIN_API_HOSTS` (the bot token goes there); requests go through `netguard` |

## The Slack app

Create an app for the workspace with:

- **OAuth & Permissions**, bot scopes `app_mentions:read`, `channels:history`, `channels:read`,
  `chat:write`, `commands`, `users:read`; redirect URL `https://<gator>/connect/callback`. Put its
  client id and secret in the server's `GATOR_SLACK_CLIENT_ID` and `GATOR_SLACK_CLIENT_SECRET`.
- **Event Subscriptions** to `https://<gator>/hooks/<project-slug>/slack`, bot events `message.channels`
  and `app_mention`.
- **Interactivity** and a slash command `/gator`, both to the same address.

Invite the bot to each channel in `channels`.

## Check live before relying on it

The tests run against a fake Web API and signed fake requests; nothing here has talked to Slack.
Before a team uses it: that `oauth.v2.access` through Gator's connect flow stores the bot token
(Slack answers `access_token` at the top level; PKCE is not required by Slack), the scopes above are
enough for `users.info` and `chat.postEphemeral`, Slack accepts JSON bodies for `chat.postMessage`,
`chat.update` and `chat.postEphemeral`, and how Slack retries a slow delivery (`X-Slack-Retry-Num`)
against the core's deduplication by body hash.
