# honeybadger

Gator's monitoring plugin for [Honeybadger](https://www.honeybadger.io). A fault becomes an
incident, and "it is over" is kept true in both places.

## What it does

| On | It does |
|---|---|
| a fault `occurred`, `reopened`, `unresolved` or `rate_exceeded` | reports an incident with the Honeybadger fault id as its fingerprint, so every notice of one fault is one incident and one task |
| a fault `resolved` | closes the incident. Its task stays open — whether the fix is finished is a person's call — but a release with nothing open against it can settle |
| the incident's task finishing | resolves the fault in Honeybadger, when `auth_token` and `project_id` are set |

Honeybadger has no severity levels, so every new fault asks at the level in `severity`
(default `high`).

## Settings

| Setting | |
|---|---|
| `webhook_token` (secret) | a long random string. Honeybadger does not sign deliveries, so the webhook URL carries it and a delivery without it is refused with 403 |
| `auth_token` (secret) | personal auth token, used only to resolve a fault when its task finishes |
| `project_id` | Honeybadger project id, needed to resolve faults |
| `api_url` | default `https://app.honeybadger.io` |
| `environment` | only faults from this environment |
| `severity` | `critical`, `high`, `medium` or `low`; default `high` |

## Webhook

In the Honeybadger project, add a **Webhook** integration pointing at:

```
https://<gator>/hooks/<project-slug>/honeybadger?token=<webhook_token>
```

The token is in the URL because Honeybadger offers nothing else. Treat that URL as a secret,
and rotate the token if it leaks.
