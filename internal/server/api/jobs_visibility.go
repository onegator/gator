package api

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store/db"
)

// A phase job is its task's work, read by the task's project. An agent's reply is said in a
// conversation — maybe a private channel or a DM — and its instruction, events and output are
// that conversation's: read by whoever may read it, by the same Access as its messages.

// replyConversation is the conversation a reply job answers in; false for any other job.
func replyConversation(ctx context.Context, q *db.Queries, job db.Job) (pgtype.UUID, bool, error) {
	if job.Kind != "reply" {
		return pgtype.UUID{}, false, nil
	}
	turn, err := q.GetTurnByJob(ctx, job.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, false, nil
	}
	if err != nil {
		return pgtype.UUID{}, false, err
	}
	return turn.ConversationID, true, nil
}

// jobReadable says whether p, who may read the job's project, may read the job itself.
func (s *Server) jobReadable(ctx context.Context, p auth.Principal, job db.Job) (bool, error) {
	conv, ok, err := replyConversation(ctx, db.New(s.Pool), job)
	if err != nil || !ok {
		return err == nil, err
	}
	if s.Conversations == nil {
		return false, nil
	}
	_, rights, err := s.Conversations.Access.RightsByID(ctx, conv, p)
	return rights.Read, err
}
