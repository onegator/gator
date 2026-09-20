# sentry

Gator's monitoring plugin for [Sentry](https://sentry.io). It turns alerts into incidents and
keeps "it is over" true in both places.

## What it does

| On | It does |
|---|---|
| an `issue` created, or an `event_alert` | reports an incident with the Sentry issue id as its fingerprint. Level decides severity: fatal → critical, error → high, warning → medium, info and debug → low. An event alert also passes its release, so the fault is blamed on what was deployed. |
| an `issue` resolved or ignored | closes the incident. Its task stays open — whether the fix is finished is a person's call — but a release with nothing open against it can settle. |
| the incident's task finishing | resolves the issue in Sentry. Without this the alert stays open there and the next occurrence looks like an old fault nobody dealt with. |

The same fault reported a thousand times is one incident and one task; that is the core's rule,
and this plugin exists to feed it a fingerprint stable enough for it to hold.

Alerts quieter than `min_level` are logged and dropped. An incident that is not worth a task is
worse than none: it teaches people to ignore the inbox.

## Settings

| Setting | |
|---|---|
| `client_secret` (secret) | the Sentry integration's client secret, which signs every webhook. A delivery that does not match is refused with 403. |
| `auth_token` (secret) | token with **event:write**, used only to resolve an issue when its task finishes. Leave it empty to receive alerts and never write back. |
| `api_url` | default `https://sentry.io/api/0` |
| `min_level` | lowest level that opens an incident: `fatal`, `error`, `warning`, `info`; default `warning` |
| `environment` | only alerts from this environment; empty means all of them |

## Webhook

Create an [internal integration](https://docs.sentry.io/organization/integrations/integration-platform/)
in Sentry, point its webhook at:

```
https://<gator>/hooks/<project-slug>/sentry
```

and enable the **issue** and **error** resources. Copy the client secret into `client_secret`
and, if the plugin should resolve issues, a token with `event:write` into `auth_token`.

## Test

```sh
go test ./sentry/
```

The tests run the real binary against an in-memory core and a fake Sentry, so they cover the
whole path: signature, deduplication by fingerprint, the level floor, and resolving in both
directions.
