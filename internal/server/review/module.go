package review

import (
	"github.com/jackc/pgx/v5/pgxpool"
)

// Deps are what the package needs from the rest of the server.
type Deps struct {
	// Blobs keeps the diffs (conversations.BlobStoreFromEnv); nil keeps them in the database.
	Blobs Blobs
	// Messages posts the notes people send to an agent (conversations.Messages), and Threads finds
	// a task's thread for them (conversations.Threads). Without them there are no comments.
	Messages poster
	Threads  taskThreads
}

// Module is the package's services wired to one database, for the server to hand out.
type Module struct {
	Checkpoints *Checkpoints
	// Comments is nil without Deps.Messages.
	Comments *Comments
}

// New wires the services.
func New(pool *pgxpool.Pool, deps Deps) *Module {
	m := &Module{Checkpoints: NewCheckpoints(pool, deps.Blobs)}
	if deps.Messages != nil && deps.Threads != nil {
		m.Comments = NewComments(pool, m.Checkpoints, deps.Messages, deps.Threads)
	}
	return m
}
