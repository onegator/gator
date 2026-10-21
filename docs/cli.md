# gator from the terminal

`gator` is a person's way to work with Gator without opening the app: sign in, see projects
and tasks, start a task, run it and watch its log as it is written.

It is not `gator-cli`. That one is the agent's tool: the runner puts it beside every job with the
job's own identity and it answers only in JSON ([agent-tools.md](agent-tools.md)). `gator` speaks
for a person, with a token of its own.

## Install

- With Gator.app: **Gator → Install Command-Line Tool…** links the `gator` inside the app into
  `~/.local/bin`. If that directory is not on your `PATH`, the app says which line to add to
  your shell profile.
- From a release: unpack `gator_<version>_<os>_<arch>.tar.gz` somewhere on your `PATH`.
- From source: `make cli` builds `bin/gator`.

## Sign in

```
gator login --server https://gator.example.com
```

opens the browser on Google's sign-in (whatever provider the server is set up with). The
browser comes back to a port on `127.0.0.1` that `gator` listens on for that one sign-in; the
code it brings is worth nothing without the PKCE verifier only this process holds, and a request
without the state `gator` sent is turned away. The token that comes back lives 90 days and is
named "gator CLI on <this machine>" in your list of tokens.

The token goes to the macOS keychain (service `dev.onegator.gator.cli`). Where there is no
keychain — Linux, CI — it goes to `~/.config/gator/credentials.json`, readable by you only.
`~/.config/gator/config.json` keeps the server and the current project and nothing secret.

It is not shared with Gator.app: each holds its own token, so signing out of one leaves the other
signed in, and either can be revoked on its own.

- `gator login --token -` reads a token made elsewhere from stdin (for a server without a
  provider, or a script).
- `gator logout` revokes the token on the server, then forgets it.
- `gator whoami` says who the token belongs to.
- `GATOR_SERVER` and `GATOR_TOKEN` override what is stored, for scripts and CI.

Inside a Gator job (`GATOR_JOB_TOKEN` set) `gator` refuses to use the stored sign-in: a runner
on your Mac runs as you, and its agent must speak as the job, not as you.

## Work

```
gator projects                                  the projects you can see, with your role
gator project create shop --repo acme/shop      you become its admin; the first one becomes current
gator use shop                                  the project the other commands default to

gator tasks [--project SLUG] [--mine] [--all]
gator task new --kind bug "Cart forgets items" --description "since Monday, Safari only"
gator task show ID
gator task advance ID [--reason TEXT]
gator task approve ID

gator task run ID [--cloud] [-f]
gator task logs ID [-f]
```

`task run` queues a job for the task's current phase. `--cloud` asks for `placement: cloud`; a
server too old to know the field refuses it, and `gator` says so instead of running locally.
`-f` (on `run` or `logs`) follows the job's output live over the WebSocket topic `job:<id>`,
falls back to polling the stored events when the socket cannot be opened, and ends when the job
does — with a non-zero exit when it did not succeed, so a script can wait on it.

## Output

Tables for people by default; `--json` on any command prints the API's objects for scripts
(`task logs --json` prints one event per line). Errors go to stderr. Exit codes match
`gator-cli`'s: `0` ok, `1` the request was wrong, `2` the server could not be reached, `3` not
signed in or the token is no longer valid, `4` the server failed, `5` the work moved on.
