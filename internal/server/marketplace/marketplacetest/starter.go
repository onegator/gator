package marketplacetest

// The starter is a small catalogue in the shape of Gator's own registry: an agent that brings a
// skill of its own and needs another, a skill on its own, a remote MCP server, a process and a
// knowledge pack. Its files never change, so every test that publishes it publishes the same
// versions with the same checksums.

// QAEngineer is the agent of the phase's first "done when": installed, @qa is there at once.
var QAEngineer = map[string]string{
	"agent.yaml": `name: qa-engineer
version: 1.2.0
display_name: QA Engineer
handle: qa
description: Writes and keeps end-to-end tests, and checks acceptance criteria before a release.
avatar: checkmark.seal
phases: [verification, release]
role_key: tester
tier: work
policy: {model: claude-sonnet-5, max_cost_usd: 3}
skills: [e2e-playwright, go-testing]
requires:
  capabilities: [vcs, ci]
triggers:
  - on: pull_request
    when: {labels: [needs-qa]}
permissions:
  - writes code on gator/* branches
  - reads CI logs
example_prompt: "@qa write an e2e test for signing in with a magic link"
`,
	"instructions.md": `You are the team's QA engineer. You make sure what is about to ship does what its task promised.

1. Read the task's acceptance criteria. If there are none, write them down first and say so in the thread.
2. Turn each criterion into a scenario: the state before, what a person does, what they must see.
3. Write the scenarios as end-to-end tests next to the existing ones, in the style already there.
4. Run them. A failing test is a finding: report it with the steps, never weaken the test to pass.
5. Say in the thread which criteria are covered, which are not, and why.
`,
	"skills/e2e-playwright/SKILL.md": `---
name: e2e-playwright
description: Write and run Playwright end-to-end tests for a web front end, one scenario per acceptance criterion.
metadata:
  version: 1.0.0
---
# End-to-end tests with Playwright

- Find the existing suite (playwright.config.*) and follow its layout and fixtures.
- One test per scenario; name it after the behaviour, not the page.
- Select by role and accessible name, never by CSS class.
- Run with ` + "`npx playwright test --reporter=line`" + `; attach the trace of a failure.
`,
	"skills/e2e-playwright/scripts/run.sh": "#!/bin/sh\nexec npx playwright test --reporter=line \"$@\"\n",
}

// GoTesting is a skill on its own, which QA Engineer needs and does not bring.
var GoTesting = map[string]string{
	"SKILL.md": `---
name: go-testing
description: Write Go tests the way this team reads them, table-driven where it helps, against a real database where it matters.
version: 1.0.0
---
# Go tests

- A test name says what holds: TestAPublishedVersionNeverChanges, not TestFetch2.
- Table tests for many inputs of one rule; a plain test for a story.
- Talk to the real database through the package's harness; never mock SQL.
- ` + "`go test -count=1 ./...`" + ` before you say it is done.
`,
}

// SentryLike is a remote MCP server signed in to with a token; the tests point it at a fake.
func SentryLike(url string) map[string]string {
	return map[string]string{"integration.yaml": `name: issues-mcp
version: 1.0.0
display_name: Issues (MCP)
description: Read the project's production errors and their stack traces.
kind: mcp
url: ` + url + `
auth: bearer
capabilities: [monitoring]
phases: [monitoring]
tools_default_allow: [search_issues, get_issue_details]
`}
}

// IncidentPostmortem is a process: an incident ends with a written postmortem.
var IncidentPostmortem = map[string]string{
	"process.yaml": `name: incident-postmortem
version: 1.0.0
display_name: Incident with a postmortem
description: An incident is fixed, verified, released, watched, and then written up.
kind: incident
template: template.yaml
roles: {postmortem: roles/postmortem.md}
phases: [monitoring]
`,
	"template.yaml": `kind: incident
max_rollbacks: 1
phases:
  - { name: implementation, owner: runner, role: worker,     gate: both,  timeout: 6h }
  - { name: verification,   owner: runner, role: reviewer,   gate: human, timeout: 4h }
  - { name: postmortem,     owner: runner, role: postmortem, gate: human, timeout: 24h }
  - { name: approved,       owner: human,  gate: auto,       timeout: 0 }
`,
	"roles/postmortem.md": "Write the incident's postmortem: what happened, when, why, what we change. No blame.\n",
}

// GoService is a knowledge pack.
var GoService = map[string]string{
	"pack.json": `{"name":"go-service","version":"1.0.0","scope":"language","applies_to":["go"],"roles":["worker","reviewer"],"files":["style.md"]}`,
	"style.md":  "# Style\nSmall packages, no stutter, errors wrapped once.\n",
	"README.md": "How this team writes Go services.\n",
}

// Starter writes the starter catalogue and publishes it. A remote MCP server's address is a
// test's own, so it is not part of the starter: SentryLike puts one in a registry of its own.
func (r *Registry) Starter() {
	r.t.Helper()
	r.Package("agents/qa-engineer", QAEngineer)
	r.Package("skills/go-testing", GoTesting)
	r.Package("process/incident-postmortem", IncidentPostmortem)
	r.Package("knowledge/go-service", GoService)
	r.Publish()
}
