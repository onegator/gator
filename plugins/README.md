# Plugins

Gator's own plugins. Each directory is one plugin: a standalone program that gator-server
starts and talks to over JSON-RPC on stdio. The protocol is in [`docs/plugins.md`](../docs/plugins.md)
and the Go SDK is the [`plugin/`](../plugin) package next door — which is why the plugins live
here: a change to the protocol and the plugins that use it land in the same commit, and a
release never pairs a plugin with a protocol it was not built against.

| Plugin | Capabilities | What it does |
|---|---|---|
| [github](github/) | tracker, vcs, ci | issues become tasks, finished jobs open pull requests, CI checks gate Implementation, phase labels |
| [sentry](sentry/) | monitoring | alerts become incidents, deduplicated by Sentry's issue id; resolving travels both ways |

A plugin written elsewhere, against the published SDK, works the same way; these are simply
the ones this repository ships.

## Develop

```sh
go test ./plugins/...   # unit and scenario tests, each against a fake of its service
make plugins-dev        # play each plugin's scenarios/*.json against an in-memory core
```

A new plugin is a directory with a `main.go` and a `scenarios/` folder; `make plugins` and
`make plugins-dev` pick it up by itself. Add it to `.goreleaser.yaml` to ship it.

## Install on a server

Every release has a `gator-plugins_<version>_<os>_<arch>.tar.gz` archive. Copy the binaries to
`/usr/local/lib/gator/plugins/`, then as a workspace admin:

```sh
curl -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  "$GATOR/api/v1/plugins/github" -d '{"command":["/usr/local/lib/gator/plugins/github"]}'
```

and enable it per project with `PUT /api/v1/projects/{id}/plugins/github`.
