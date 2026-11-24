// Package agents makes agents members of a project rather than functions of it. An agent has an
// identity people mention (@reviewer), a profile, a role it plays in the process, a tier that says
// what it may touch, a budget and a state; everything it does is recorded as its work and as the
// work of the person who asked for it.
//
// The roles of the process (process/roles) are the default agents of every project; the database
// seeds them with the project (migration 00039), so a project made in any way has them. A process
// template keeps saying role: reviewer, and the project's agent for that role does the job.
package agents

import "github.com/jackc/pgx/v5/pgtype"

// Tiers: what an agent may touch when it answers in a conversation.
const (
	// TierChat reads the repository, searches and answers, through tools that do not write.
	TierChat = "chat"
	// TierWork changes code on a gator/ branch and runs commands, like a phase job; it needs a
	// person's consent on the task.
	TierWork = "work"
)

// States of an agent. A paused agent refuses turns and says so.
const (
	StatusActive = "active"
	StatusPaused = "paused"
)

// Sources: where an agent comes from. A built-in one cannot be deleted, only paused.
const (
	SourceBuiltin     = "builtin"
	SourceCustom      = "custom"
	SourceMarketplace = "marketplace"
)

// Default is one of the agents every project starts with.
type Default struct {
	Handle      string
	DisplayName string
	RoleKey     string
	Tier        string
}

// Defaults are the agents gator_seed_default_agents (migration 00039) gives every project, in
// the order of the process. They are written down here for the code that names them; a test
// keeps the two lists the same.
var Defaults = []Default{
	{Handle: "researcher", DisplayName: "Researcher", RoleKey: "researcher", Tier: TierChat},
	{Handle: "planner", DisplayName: "Planner", RoleKey: "planner", Tier: TierChat},
	{Handle: "worker", DisplayName: "Worker", RoleKey: "worker", Tier: TierWork},
	{Handle: "reviewer", DisplayName: "Reviewer", RoleKey: "reviewer", Tier: TierChat},
	{Handle: "security", DisplayName: "Security", RoleKey: "security_reviewer", Tier: TierChat},
	{Handle: "curator", DisplayName: "Curator", RoleKey: "curator", Tier: TierChat},
	{Handle: "triage", DisplayName: "Triage", RoleKey: "triage", Tier: TierWork},
	{Handle: "reporter", DisplayName: "Reporter", RoleKey: "reporter", Tier: TierChat},
}

func uuidText(u pgtype.UUID) string {
	v, _ := u.Value()
	s, _ := v.(string)
	return s
}
