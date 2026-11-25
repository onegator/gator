package api

import (
	"github.com/onegator/gator/internal/server/jobscope"
	"github.com/onegator/gator/internal/server/store/db"
)

// agentCommand is one call of the agent surface, named as gator-cli names it.
type agentCommand = jobscope.Command

const (
	cmdTaskShow       = jobscope.TaskShow
	cmdTaskBlocked    = jobscope.TaskBlocked
	cmdAsk            = jobscope.Ask
	cmdArtifactPut    = jobscope.ArtifactPut
	cmdProductPropose = jobscope.ProductPropose
	cmdCatalogueShow  = jobscope.CatalogueShow
	cmdKnowledge      = jobscope.Knowledge
	cmdSay            = jobscope.Say
	cmdThreadShow     = jobscope.ThreadShow
	cmdTaskCreate     = jobscope.TaskCreate
	cmdAttachmentRead = jobscope.AttachmentRead
)

// scopeOf is what a job's own identity may call, by the job's kind (jobscope.For, which the MCP
// server asks too).
func scopeOf(job db.Job) jobscope.Set { return jobscope.For(job.Kind) }
