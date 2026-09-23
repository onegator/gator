# What an agent can say back

An agent used to get a prompt and hand back a receipt. That was the whole conversation: it
could not say the task was impossible, could not ask a question, could not write down what it
learned. It guessed, and the guess arrived an hour later as a report.

`gator-cli` is the other half. The runner puts it on the path of every job with that job's own
identity, so an agent can talk to Gator about the work it was given — and about nothing else.

## The commands

```
gator-cli task show                     the task, its phase, its gate, the documents earlier phases produced
gator-cli task blocked --reason TEXT    stop and ask a person, instead of guessing
gator-cli ask --question TEXT           ask a question and stop there
gator-cli artifact put --type TYPE --file -    record a document now, so it survives a failed job
gator-cli product propose --kind decision|lesson --title TEXT --file -
gator-cli catalogue show                the parts this project is made of
gator-cli knowledge search --query TEXT what the project knows, beyond what this prompt carried
```

Output is JSON on stdout, one object or one array. Problems are `{"error": "..."}` on stderr.
The exit code says what kind of problem it was: `0` ok, `1` the request was wrong, `2` Gator
could not be reached, `3` no identity or an expired one, `4` the server failed, `5` the work
moved on. Anything that creates something answers with its id and a `gator://tasks/<id>` link,
which the agent is asked to quote in its report so a person can open it.

Long text goes in through `--file -` and stdin, because an agent fighting its own shell quoting
gives up and writes something shorter.

## The identity

Every leased job is given a token of its own, minted when the job is leased and revoked when it
ends. Its scope is the job: it reaches the agent commands and nothing else, and nothing outside
the job's project. That matters because an agent reads words written by people outside this
workspace — a GitHub issue, a monitoring alert — so whatever an agent can reach is whatever
those words can reach. See [the origin of a task](../internal/server/store/migrations/00022_task_origin.sql).

A token that leaks out of a worktree can still only discuss one task, and only while that task's
job is running.

## What an agent cannot do

Approve anything. `product propose` records a proposal; a person decides what the product knows
about itself, because every later prompt carries it. The same reasoning applies to the
catalogue: an agent reads it, a person writes it.

Move a task on. Advancing a phase is a decision with a gate, and the gate belongs to people and
plugins. An agent that thinks the work is done says so in its report; an agent that thinks it
cannot go on blocks the task and says why.

## Where the calls are recorded

In `agent_calls`, next to the job: what was called, with what, and when. Plugins have had this
since M3. Agents talk more often, and the whole point of letting them talk is that a person can
read what was said.

## Running without it

If the runner cannot find `gator-cli` beside itself, or the server mints no job identity, the
agent gets no tools and the prompt says nothing about them. Jobs run exactly as they did before.
