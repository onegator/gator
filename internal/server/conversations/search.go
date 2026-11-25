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
	// candidates is how many matches are read per hit wanted before the access rules leave the
	// readable ones, and searchRounds how many such batches at most.
	candidates   = 4
	searchRounds = 5
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
	Message View
	// Conversation is where it was said, with the caller's standing there (joined, read up to).
	Conversation Listed
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
	readable := map[[16]byte]*db.Conversation{}
	var msgs []db.Message
	batch := int32(limit * candidates)
	// The access rules can leave out most of a batch (a busy private channel the SQL could not
	// rule out); read further batches until the page is full or the matches run out.
	for round, skip := 0, int32(0); round < searchRounds && len(msgs) < limit; round, skip = round+1, skip+batch {
		rows, err := q.SearchMessages(ctx, db.SearchMessagesParams{
			Query: text, ProjectID: in.Project, AllProjects: scope.All, ProjectIds: projects, UserID: me.ID, MaxRows: batch, Skip: skip,
		})
		if err != nil {
			return nil, err
		}
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
		if len(rows) < int(batch) {
			break
		}
	}
	views, err := describe(ctx, q, msgs)
	if err != nil {
		return nil, err
	}
	// The caller's standing in the page's conversations, in one query.
	ids := make([]pgtype.UUID, 0, len(readable))
	for id, c := range readable {
		if c != nil {
			ids = append(ids, pgtype.UUID{Bytes: id, Valid: true})
		}
	}
	standing := map[[16]byte]db.ListMembershipsInRow{}
	if me.ID.Valid && len(ids) > 0 {
		rows, err := q.ListMembershipsIn(ctx, db.ListMembershipsInParams{UserID: me.ID, ConversationIds: ids})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			standing[r.ConversationID.Bytes] = r
		}
	}
	out := make([]Hit, 0, len(views))
	for _, v := range views {
		c := *readable[v.Message.ConversationID.Bytes]
		m, joined := standing[c.ID.Bytes]
		out = append(out, Hit{Message: v, Conversation: listed(c, joined, m.LastReadSeq, m.Notify, m.UnreadMentions)})
	}
	return out, nil
}
