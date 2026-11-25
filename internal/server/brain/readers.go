package brain

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store/db"
)

// readerStore is what Readers reads. *db.Queries is one.
type readerStore interface {
	ListMembershipsForUser(ctx context.Context, userID pgtype.UUID) ([]db.Membership, error)
	GetUser(ctx context.Context, id pgtype.UUID) (db.User, error)
	GetJob(ctx context.Context, id pgtype.UUID) (db.Job, error)
	GetTurnByJob(ctx context.Context, jobID pgtype.UUID) (db.AgentTurn, error)
}

// Readers works out who reads: a person from their memberships, a job from its project and the
// person its turn acts for.
type Readers struct{ store readerStore }

// NewReaders builds it.
func NewReaders(store readerStore) Readers { return Readers{store: store} }

// ForPrincipal is the reader behind a request: a person, or a job's own identity. Runners and
// plugins read nothing through the Brain's doors.
func (rs Readers) ForPrincipal(ctx context.Context, p auth.Principal) (Reader, error) {
	switch p.Kind {
	case auth.KindUser:
		return rs.person(ctx, p.UserID, p.IsWorkspaceAdmin())
	case auth.KindJob:
		job, err := rs.store.GetJob(ctx, p.JobID)
		if err != nil {
			return Reader{}, err
		}
		return rs.ForJob(ctx, job)
	}
	return Reader{}, ErrForbidden
}

// ForUser is a person who is not the caller, as the rules see them.
func (rs Readers) ForUser(ctx context.Context, user pgtype.UUID) (Reader, error) {
	u, err := rs.store.GetUser(ctx, user)
	if err != nil {
		return Reader{}, err
	}
	return rs.person(ctx, u.ID, u.WorkspaceRole == "admin")
}

func (rs Readers) person(ctx context.Context, user pgtype.UUID, admin bool) (Reader, error) {
	ms, err := rs.store.ListMembershipsForUser(ctx, user)
	if err != nil {
		return Reader{}, err
	}
	r := Reader{User: user, Admin: admin, Roles: map[[16]byte]auth.Role{}}
	for _, m := range ms {
		r.Roles[m.ProjectID.Bytes] = auth.ParseRole(m.Role)
	}
	return r, nil
}

// ForJob is an agent reading from inside job: a member of the job's project, lent the private
// context of the person its turn acts for and of nobody else.
func (rs Readers) ForJob(ctx context.Context, job db.Job) (Reader, error) {
	r := Reader{Job: job.ID, Project: job.ProjectID, Roles: map[[16]byte]auth.Role{job.ProjectID.Bytes: auth.RoleMember}}
	if !job.TurnID.Valid {
		return r, nil
	}
	turn, err := rs.store.GetTurnByJob(ctx, job.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, nil
	}
	if err != nil {
		return Reader{}, err
	}
	r.User = turn.OnBehalfOf
	return r, nil
}
