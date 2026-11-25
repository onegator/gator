package api

import (
	"github.com/onegator/gator/internal/server/runners"
	"github.com/onegator/gator/internal/server/store/db"
)

// agentCommand is one call of the agent surface, named as gator-cli names it.
type agentCommand string

const (
	cmdTaskShow       agentCommand = "task show"
	cmdTaskBlocked    agentCommand = "task blocked"
	cmdAsk            agentCommand = "ask"
	cmdArtifactPut    agentCommand = "artifact put"
	cmdProductPropose agentCommand = "product propose"
	cmdCatalogueShow  agentCommand = "catalogue show"
	cmdKnowledge      agentCommand = "knowledge search"
	cmdSay            agentCommand = "say"
	cmdThreadShow     agentCommand = "thread show"
	cmdTaskCreate     agentCommand = "task create"
)

// commands is a set of them.
type commands map[agentCommand]bool

func setOf(cs ...agentCommand) commands {
	out := commands{}
	for _, c := range cs {
		out[c] = true
	}
	return out
}

// A phase job works on its phase: it blocks the gate, asks, records documents. A reply answers in
// a conversation and touches no phase — consented or not, blocking a gate or writing its
// documents is not what anyone asked it for — so it talks, looks things up and proposes. A
// summary is a cheap model reading a transcript with no identity at all.
var (
	phaseCommands = setOf(cmdTaskShow, cmdTaskBlocked, cmdAsk, cmdArtifactPut, cmdProductPropose, cmdCatalogueShow,
		cmdKnowledge, cmdSay, cmdThreadShow, cmdTaskCreate)
	replyCommands = setOf(cmdSay, cmdThreadShow, cmdTaskCreate, cmdKnowledge, cmdCatalogueShow, cmdProductPropose)
)

// scopeOf is what a job's own identity may call, by the job's kind; anything unknown may call
// nothing.
func scopeOf(job db.Job) commands {
	switch job.Kind {
	case runners.KindPhase:
		return phaseCommands
	case runners.KindReply:
		return replyCommands
	}
	return commands{}
}
