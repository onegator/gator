package conversations

import (
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/store/db"
)

// Module is the package's services wired to one database, for the server to hand out. It does
// nothing itself.
type Module struct {
	Access    *Access
	Directory *Directory
	Messages  *Messages
	Threads   *Threads
}

// New wires the services. roles answers project roles (auth.Authorizer).
func New(pool *pgxpool.Pool, roles RoleResolver) *Module {
	access := NewAccess(roles, db.New(pool))
	return &Module{
		Access:    access,
		Directory: NewDirectory(pool, access, roles),
		Messages:  NewMessages(pool, access),
		Threads:   NewThreads(pool, access),
	}
}
