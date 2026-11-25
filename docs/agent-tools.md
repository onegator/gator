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
gator-cli say --file -                  say something in the conversation; @agent asks another agent
gator-cli thread show [--limit N]       more of the conversation than the prompt carried
gator-cli task create --kind KIND --title TEXT --file -    propose a task; a person accepts it
```

The last three are about the conversation the job speaks in: the one its turn answers in, or
for a phase job, its task's thread. What `say` posts is the agent's own message in its turn, so
an `@handle` of another agent there is a delegation in the same chain, under its limits (four
turns, a budget of twice a turn of the asked agent, never the same agent twice in a row, a work
agent only with a person's consent on the task). `task create` posts a proposal card; nothing
is created until a person accepts it (`POST /proposals/{messageId}/accept`).

What a job's identity may call is its kind's, checked by the server. A phase job may call all of
the above. A reply (an agent answering in a conversation, chat or work) may `say`, `thread show`,
`task create`, `knowledge search`, `catalogue show` and `product propose`; it never blocks a gate,
asks a phase's question or writes a phase's documents. A summary job gets no identity at all.

An agent answering in chat (`tier: chat`) has no tools that write and a read-only checkout; it
may run `gator-cli` and nothing else. A summary has no tools and no checkout.

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

Approve anything. `product propose` records a proposal, `task create` a proposed task; a person decides what the product knows
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

## The same, over MCP

gator-server also serves the Brain and the agent surface as an MCP server, `gator`, at `POST /mcp`
(Streamable HTTP, stateless). The runner adds it to every session that has an identity, beside
the runner's own `--mcp-config`, in a file only the runner's user reads (the token is never on the
command line); a read-only `chat` session may call its tools (`mcp__gator`).

| Tool | Like | A reply may |
|---|---|---|
| `brain_search(query, scope?, kind?, limit?)` | `knowledge search` | yes |
| `brain_get(id)` | `knowledge search` | yes |
| `brain_propose(kind, title, content, scope?, component?, links?)` | `product propose` | yes |
| `brain_cite(ids)` | — | yes |
| `catalogue_show(component?)` | `catalogue show` | yes |
| `thread_read(conversation_id?, limit?)` | `thread show` | yes |
| `conversation_post(body)` | `say` | yes |
| `task_show()`, `task_blocked(reason)`, `ask(question)` | the same | no |

A token reaches the same tools over MCP as over `/agent` (both ask `internal/server/jobscope`); a
summary job has none. Every call is in `agent_calls`, as `mcp <tool>`. An entry the agent relies on
is cited as `[brain:<id>]`: the package of every job carries those tags, and what a job's final
message cites of what it was handed or looked up is recorded in `brain_citations`.

A person connects their own client with their token and reads what they read in the app:
`claude mcp add --transport http gator https://<server>/mcp --header "Authorization: Bearer gtr_user_…"`.
They get `brain_search`, `brain_get`, `brain_propose` (always a proposal), `brain_cite` and
`catalogue_show(component?, project)`, and the resources `brain://me` and
`brain://project/<slug>/principles`.
