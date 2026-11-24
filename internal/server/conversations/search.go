package conversations

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store/db"
)

const (
	defaultHits  = 20
	maxHits      = 100
	maxQueryRune = 200
	// candidates is how many matches are read before the access rules leave the readable ones.
	candidates = 4
)

// Search finds messages by their words, among the conversations the caller may read.
type Search struct {
	pool   *pgxpool.Pool
	access *Access
}

// NewSearch builds the search.
func NewSearch(pool *pgxpool.Pool, access *Access) *Search {
	return &Search{pool: pool, access: access}
}

// Scope is where the caller may look: every project (workspace admins) or these.
type Scope struct {
	All      bool
	Projects []pgtype.UUID
}

// Query is one search.
type Query struct {
	Text string
	// Project narrows it to one project's conversations.
	Project pgtype.UUID
	Limit   int
}

// Hit is a message found and the conversation it is in.
type Hit struct {
	Message      View
	Conversation db.Conversation
}

// Messages answers the messages that match, best first. Every hit passes the same access rules as
// reading the conversation it is in.
func (s *Search) Messages(ctx context.Context, p auth.Principal, scope Scope, in Query) ([]Hit, error) {
	text := strings.TrimSpace(in.Text)
	if text == "" || utf8.RuneCountInString(text) > maxQueryRune {
		return nil, invalid("a search is 1–%d characters", maxQueryRune)
	}
	limit := in.Limit
	if limit <= 0 {
		limit = defaultHits
	}
	limit = min(limit, maxHits)
	me, _ := memberOf(p)
	projects := scope.Projects
	if projects == nil {
		projects = []pgtype.UUID{}
	}
	q := db.New(s.pool)
	rows, err := q.SearchMessages(ctx, db.SearchMessagesParams{
		Query: text, ProjectID: in.Project, AllProjects: scope.All, ProjectIds: projects, UserID: me.ID, MaxRows: int32(limit * candidates),
	})
	if err != nil {
		return nil, err
	}
	readable := map[[16]byte]*db.Conversation{}
	var msgs []db.Message
	for _, r := range rows {
		conv, seen := readable[r.Message.ConversationID.Bytes]
		if !seen {
			c, rights, err := s.access.RightsByID(ctx, r.Message.ConversationID, p)
			if err != nil {
				return nil, err
			}
			if rights.Read {
				conv = &c
			}
			readable[r.Message.ConversationID.Bytes] = conv
		}
		if conv != nil && len(msgs) < limit {
			msgs = append(msgs, r.Message)
		}
	}
	views, err := describe(ctx, q, msgs)
	if err != nil {
		return nil, err
	}
	out := make([]Hit, 0, len(views))
	for _, v := range views {
		out = append(out, Hit{Message: v, Conversation: *readable[v.Message.ConversationID.Bytes]})
	}
	return out, nil
}
