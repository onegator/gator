# Go in this workspace

- A package name says what it holds; the type inside does not repeat it (`knowledge.Pack`, not `knowledge.KnowledgePack`).
- Return errors with context the reader can act on: `fmt.Errorf("registry clone: %w: %s", err, output)`.
- Comments say why, never what the next line already says. A comment that restates the code is deleted.
- Exported identifiers carry a doc comment that starts with their name.
- Keep the happy path at the left margin: handle the error and return.
- Prefer the standard library. A dependency has to earn its place in go.mod.
