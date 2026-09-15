# gator-plugins

Plugins for [Gator](https://github.com/onegator/gator). Each directory is one plugin: a
standalone program that gator-server starts and talks to over JSON-RPC on stdio. The protocol
and the Go SDK are in gator's `docs/plugins.md` and `plugin/` package.

| Plugin | Capabilities | What it does |
|---|---|---|
| [github](github/) | tracker, vcs, ci | issues become tasks, finished jobs open pull requests, CI checks gate Implementation, phase labels |

## Develop

The SDK lives in the private `onegator/gator` module, so tell Go and git about it once:

```sh
go env -w GOPRIVATE=github.com/onegator/*
```

Then:

```sh
make test   # unit and scenario tests, with a fake GitHub API
make dev    # play each plugin's scenarios/*.json against an in-memory core
```

CI needs a `GATOR_READ_TOKEN` repository secret: a fine-grained token with Contents: read
on `onegator/gator`.

## Install on a server

Build for the server and copy the binary to `/usr/local/lib/gator/plugins/`, then as a
workspace admin:

```sh
curl -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  "$GATOR/api/v1/plugins/github" -d '{"command":["/usr/local/lib/gator/plugins/github"]}'
```

and enable it per project with `PUT /api/v1/projects/{id}/plugins/github`.
