package review

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

// Comment is a comment on the lines of a checkpoint's diff.
type Comment = db.ReviewComment

// poster posts a message as a person, under the conversation's rules. conversations.Messages is one.
type poster interface {
	Post(ctx context.Context, p auth.Principal, conversation pgtype.UUID, d conversations.Draft) (conversations.Posted, error)
}

// taskThreads answers a task's thread. conversations.Threads is one.
type taskThreads interface {
	ForTask(ctx context.Context, task db.Task) (db.Conversation, error)
}

// Comments are what people say about the lines of a turn's diff: drafts of their own until they
// send them to an agent, in one message in the conversation where the agent works, which asks the
// agent (with the consent a work agent needs, like any mention) and carries the comments as
// structure: the agent reads them, with the code they are about, in the package of its turn.
//
// Who may do it is the caller's to check first (a member of the job's project who may read the
// job): Comments holds the rest — only a person comments, only on lines the diff shows, edits only
// their own drafts, and sends only their own.
type Comments struct {
	pool        *pgxpool.Pool
	checkpoints *Checkpoints
	messages    poster
	threads     taskThreads
}

// NewComments builds it.
func NewComments(pool *pgxpool.Pool, checkpoints *Checkpoints, messages poster, threads taskThreads) *Comments {
	return &Comments{pool: pool, checkpoints: checkpoints, messages: messages, threads: threads}
}

// NewComment is a comment as a person writes it.
type NewComment struct {
	Path      string
	Side      string
	LineStart int
	LineEnd   int
	Body      string
}

const maxCommentRunes = 8000

// Create keeps a draft comment on lines of a checkpoint's diff (its turn's, else its job's so
// far), with the code it is about.
func (c *Comments) Create(ctx context.Context, p auth.Principal, checkpoint pgtype.UUID, in NewComment) (Comment, error) {
	if p.Kind != auth.KindUser || !p.UserID.Valid {
		return Comment{}, fmt.Errorf("%w: only a person comments on changes", ErrForbidden)
	}
	in.Body = strings.TrimSpace(in.Body)
	if in.LineEnd == 0 {
		in.LineEnd = in.LineStart
	}
	switch {
	case in.Side != "old" && in.Side != "new":
		return Comment{}, fmt.Errorf("%w: side is old or new", ErrInvalid)
	case in.LineStart < 1 || in.LineEnd < in.LineStart || in.LineEnd-in.LineStart >= 500:
		return Comment{}, fmt.Errorf("%w: lines %d–%d", ErrInvalid, in.LineStart, in.LineEnd)
	case in.Body == "" || utf8.RuneCountInString(in.Body) > maxCommentRunes:
		return Comment{}, fmt.Errorf("%w: a comment is 1 to %d characters", ErrInvalid, maxCommentRunes)
	case in.Path == "" || len(in.Path) > maxPath:
		return Comment{}, fmt.Errorf("%w: path", ErrInvalid)
	}
	snippet, ok := "", false
	for _, which := range []Which{TurnDiff, WholeDiff} {
		diff, err := c.checkpoints.Diff(ctx, checkpoint, which)
		if err != nil {
			return Comment{}, err
		}
		if snippet, ok = Snippet(diff, in.Path, in.Side, in.LineStart, in.LineEnd); ok {
			break
		}
	}
	if !ok {
		return Comment{}, fmt.Errorf("%w: the diff does not show lines %d–%d of %s", ErrInvalid, in.LineStart, in.LineEnd, in.Path)
	}
	return db.New(c.pool).InsertReviewComment(ctx, db.InsertReviewCommentParams{CheckpointID: checkpoint, AuthorID: p.UserID,
		Path: in.Path, Side: in.Side, LineStart: int32(in.LineStart), LineEnd: int32(in.LineEnd), Body: in.Body, Snippet: snippet})
}

// JobComment is a comment with the turn it is on.
type JobComment = db.ListJobReviewCommentsRow

// List answers a job's comments for one person: every comment sent, and their own drafts.
func (c *Comments) List(ctx context.Context, p auth.Principal, job pgtype.UUID) ([]JobComment, error) {
	return db.New(c.pool).ListJobReviewComments(ctx, db.ListJobReviewCommentsParams{JobID: job, Viewer: p.UserID})
}

// Get answers one comment: a sent one, or the caller's own draft.
func (c *Comments) Get(ctx context.Context, p auth.Principal, id pgtype.UUID) (Comment, error) {
	cm, err := db.New(c.pool).GetReviewComment(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !cm.MessageID.Valid && cm.AuthorID != p.UserID) {
		return Comment{}, ErrNotFound
	}
	return cm, err
}

// Change is what a person changes of a comment; nil leaves it.
type Change struct {
	Body     *string
	Resolved *bool
}

// Update changes a comment: its body, by its author while it is a draft; whether it is resolved,
// by anyone who may comment on the job.
func (c *Comments) Update(ctx context.Context, p auth.Principal, id pgtype.UUID, ch Change) (Comment, error) {
	var out Comment
	err := store.InTx(ctx, c.pool, func(q *db.Queries) error {
		cm, err := q.GetReviewCommentForUpdate(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !cm.MessageID.Valid && cm.AuthorID != p.UserID) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		body, resolved := cm.Body, cm.ResolvedAt.Valid
		if ch.Body != nil {
			if cm.AuthorID != p.UserID {
				return fmt.Errorf("%w: only its author changes what a comment says", ErrForbidden)
			}
			if cm.MessageID.Valid {
				return fmt.Errorf("%w: this comment was sent; write another", ErrInvalid)
			}
			body = strings.TrimSpace(*ch.Body)
			if body == "" || utf8.RuneCountInString(body) > maxCommentRunes {
				return fmt.Errorf("%w: a comment is 1 to %d characters", ErrInvalid, maxCommentRunes)
			}
		}
		if ch.Resolved != nil {
			resolved = *ch.Resolved
		}
		out, err = q.UpdateReviewComment(ctx, db.UpdateReviewCommentParams{ID: id, Body: body, Resolved: resolved})
		return err
	})
	return out, err
}

// Delete takes back one of the caller's drafts.
func (c *Comments) Delete(ctx context.Context, p auth.Principal, id pgtype.UUID) error {
	cm, err := c.Get(ctx, p, id)
	if err != nil {
		return err
	}
	if cm.AuthorID != p.UserID || cm.MessageID.Valid {
		return fmt.Errorf("%w: only a draft of one's own is taken back", ErrForbidden)
	}
	return db.New(c.pool).DeleteReviewComment(ctx, id)
}

// Send posts the caller's drafts on a job as one message, mentioning the agent (the job's, or
// agent), where the job's agent works: the conversation of its turn, else the task's thread. The
// drafts are taken under a lock in the message's own transaction, so two sends never post the
// same comment twice and a send that fails leaves them drafts.
func (c *Comments) Send(ctx context.Context, p auth.Principal, job db.Job, agent pgtype.UUID) (conversations.Posted, int, error) {
	if !agent.Valid {
		agent = job.AgentID
	}
	if !agent.Valid {
		return conversations.Posted{}, 0, fmt.Errorf("%w: name the agent the notes are for", ErrInvalid)
	}
	q := db.New(c.pool)
	a, err := q.GetAgent(ctx, agent)
	if errors.Is(err, pgx.ErrNoRows) {
		return conversations.Posted{}, 0, fmt.Errorf("%w: no such agent", ErrInvalid)
	}
	if err != nil {
		return conversations.Posted{}, 0, err
	}
	conv, err := c.home(ctx, q, job)
	if err != nil {
		return conversations.Posted{}, 0, err
	}
	var locked []db.ReviewComment
	sent := 0
	posted, err := c.messages.Post(ctx, p, conv.ID, conversations.Draft{
		// The drafts as they are now, locked: the message says what it carries, and one sent
		// meanwhile goes with the other message.
		Compose: func(ctx context.Context, q *db.Queries) (string, error) {
			var err error
			if locked, err = q.ListDraftReviewComments(ctx, db.ListDraftReviewCommentsParams{JobID: job.ID, AuthorID: p.UserID}); err != nil {
				return "", err
			}
			if len(locked) == 0 {
				return "", fmt.Errorf("%w: there are no drafts of yours on this job", ErrInvalid)
			}
			return notesMessage(a.Handle, locked), nil
		},
		Mentions: []conversations.Member{{Kind: conversations.MemberAgent, ID: a.ID}},
		Within: func(ctx context.Context, q *db.Queries, msg db.Message) error {
			ids := make([]pgtype.UUID, len(locked))
			for i, d := range locked {
				ids[i] = d.ID
			}
			sent = len(ids)
			return q.SendReviewComments(ctx, db.SendReviewCommentsParams{MessageID: msg.ID, Ids: ids})
		},
	})
	return posted, sent, err
}

// home is where notes on a job go: where its agent answered, or its task's thread.
func (c *Comments) home(ctx context.Context, q *db.Queries, job db.Job) (db.Conversation, error) {
	if job.TurnID.Valid {
		turn, err := q.GetTurn(ctx, job.TurnID)
		if err == nil {
			return q.GetConversation(ctx, turn.ConversationID)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return db.Conversation{}, err
		}
	}
	task, err := q.GetTask(ctx, job.TaskID)
	if err != nil {
		return db.Conversation{}, err
	}
	return c.threads.ForTask(ctx, task)
}

// notesMessage is what people read of the notes in the conversation; the agent reads them whole,
// as structure, in its package.
func notesMessage(handle string, drafts []db.ReviewComment) string {
	var b strings.Builder
	word := "notes"
	if len(drafts) == 1 {
		word = "note"
	}
	fmt.Fprintf(&b, "@%s, %d %s on your changes:", handle, len(drafts), word)
	for _, d := range drafts {
		fmt.Fprintf(&b, "\n- `%s` %s: %s", inlinePath(d.Path), lineRange(int(d.LineStart), int(d.LineEnd)), firstLine(d.Body))
	}
	return b.String()
}

func lineRange(start, end int) string {
	if end <= start {
		return fmt.Sprintf("line %d", start)
	}
	return fmt.Sprintf("lines %d–%d", start, end)
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if utf8.RuneCountInString(l) > 160 {
		r := []rune(l)
		l = string(r[:160]) + "…"
	}
	return l
}

// inlinePath is a path from the agent's diff as one line of the person's message: no line breaks,
// no backticks to end the code span, no fence-like text, bounded. The agent reads the path whole,
// fenced, in its notes.
func inlinePath(p string) string {
	p = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '`' {
			return '_'
		}
		return r
	}, p)
	p = strings.ReplaceAll(p, "<<<", "<‹<")
	if r := []rune(p); len(r) > 200 {
		p = "…" + string(r[len(r)-200:])
	}
	return p
}
