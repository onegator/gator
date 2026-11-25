package review

import (
	"github.com/jackc/pgx/v5/pgxpool"
)

// Deps are what the package needs from the rest of the server.
type Deps struct {
	// Blobs keeps the diffs (conversations.BlobStoreFromEnv); nil keeps them in the database.
	Blobs Blobs
}

// Module is the package's services wired to one database, for the server to hand out.
type Module struct {
	Checkpoints *Checkpoints
}

// New wires the services.
func New(pool *pgxpool.Pool, deps Deps) *Module {
	return &Module{Checkpoints: NewCheckpoints(pool, deps.Blobs)}
}
