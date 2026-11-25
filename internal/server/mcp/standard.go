package mcp

import (
	"log/slog"
	"slices"

	"github.com/onegator/gator/internal/server/brain"
	"github.com/onegator/gator/internal/server/store/db"
)

// Deps are what the standard server is made of.
type Deps struct {
	Brain   *brain.Module
	Queries *db.Queries
	// Roles answers a person's role in a project (auth.Authorizer).
	Roles roles
	// Talk is the agents' side of conversations (agents.Talk); nil: no conversation tools, and
	// proposals say nothing of where they were made.
	Talk interface {
		talk
		origins
	}
	// Tasks is the process (process.Service).
	Tasks tasks
	Log   *slog.Logger
}

// Standard is the gator-brain server with every tool and resource Gator has.
func Standard(d Deps) *Server {
	var from origins = noOrigin{}
	tools := slices.Concat(CatalogueTools(d.Queries, d.Roles, d.Queries), TaskTools(d.Tasks))
	if d.Talk != nil {
		from = d.Talk
		tools = append(tools, ConversationTools(d.Talk)...)
	}
	tools = append(BrainTools(d.Brain.Finder, d.Brain.Service, d.Brain.Citations, from, d.Queries), tools...)
	return New(d.Queries, d.Brain.Readers, tools, Resources(d.Brain.Finder, d.Brain.Service, d.Queries), d.Log)
}
