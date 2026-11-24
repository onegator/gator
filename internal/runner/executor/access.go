package executor

import (
	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/backend"
)

// talkBack is the one command a read-only agent may run: gator-cli, to say something in its
// conversation, read more of it, or propose a task.
const talkBack = "gator-cli"

// AccessFor is what a job's agent may do, by its tier: a chat job reads and answers with tools
// that do not write (and may run gator-cli when it has an identity to talk back with); a work
// job, like every job before tiers, may do anything in its worktree.
func AccessFor(job proto.Job) backend.Access {
	if !job.ReadOnly() {
		return backend.Access{}
	}
	a := backend.Access{ReadOnly: true}
	if job.AgentToken != "" {
		a.Commands = []string{talkBack}
	}
	return a
}

// trailerFor is what every commit of the job says about who made it; nothing for a job no agent
// signed.
func trailerFor(job proto.Job) string {
	if job.Agent == nil || job.Agent.Handle == "" {
		return ""
	}
	return "Co-Authored-By: @" + job.Agent.Handle + " via Gator"
}
