# Tests in this workspace

- A test name says what must hold: `TestAPackThatDoesNotMatchItsChecksumIsRefused`.
- Test behaviour through the exported surface; reach for internals only when nothing else can see it.
- A failure message prints what was expected and what happened, so the log alone explains it.
- No sleeping on a hope: poll for the condition with a deadline, or drive the clock.
- Anything that touches the database, git or a process gets a temporary one of its own and cleans it up.
