package conversations

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

// TaskCreator creates tasks. process.Service is one: a task made from a message goes through
// the same door as any other, with its consent and origin rules.
type TaskCreator interface {
	Create(ctx context.Context, p process.CreateParams, actor process.Actor) (db.Task, error)
}

// PromotableKinds are the kinds of task a message can become.
var PromotableKinds = []string{"feature", "bug", "chore", "incident"}

// Limits of what a promotion copies into the task.
const (
	maxTitleRunes    = 80
	maxQuotedRunes   = 1500
	maxReplyRunes    = 300
	maxQuotedReplies = 5
)

// Promoter makes a message into a task: "make this a task". The thread the message started (or
// the thread it is a reply in) becomes the task's thread, and the task's description quotes the
// conversation and links back to it.
type Promoter struct {
	pool    *pgxpool.Pool
	access  *Access
	roles   RoleResolver
	tasks   TaskCreator
	threads taskThreads
}

// NewPromoter builds the promoter.
func NewPromoter(pool *pgxpool.Pool, access *Access, roles RoleResolver, tasks TaskCreator, threads taskThreads) *Promoter {
	return &Promoter{pool: pool, access: access, roles: roles, tasks: tasks, threads: threads}
}

// Promotion says what task a message becomes.
type Promotion struct {
	Kind string
	// Title defaults to the message's first line.
	Title string
	// Project is needed only for a message in a DM outside every project.
	Project pgtype.UUID
}

// Promoted is the task a message became and its thread.
type Promoted struct {
	Task         db.Task
	Conversation pgtype.UUID
}

// Promote makes the message into a task, as actor. The caller must be able to write where the
// message is and in the project the task goes to.
func (p *Promoter) Promote(ctx context.Context, caller auth.Principal, actor process.Actor, message pgtype.UUID, in Promotion) (Promoted, error) {
	q := db.New(p.pool)
	msg, err := q.GetMessage(ctx, message)
	if err != nil {
		return Promoted{}, err
	}
	conv, rights, err := p.access.RightsByID(ctx, msg.ConversationID, caller)
	if err != nil {
		return Promoted{}, err
	}
	if !rights.Read {
		return Promoted{}, ErrNotFound
	}
	if !rights.Write {
		return Promoted{}, ErrForbidden
	}
	if msg.DeletedAt.Valid {
		return Promoted{}, invalid("a deleted message cannot become a task")
	}
	if !slices.Contains(PromotableKinds, in.Kind) {
		return Promoted{}, invalid("a message becomes a %s", strings.Join(PromotableKinds, ", "))
	}
	project, err := p.project(ctx, caller, conv, in.Project)
	if err != nil {
		return Promoted{}, err
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = firstLine(msg.Body, maxTitleRunes)
	}
	if title == "" {
		return Promoted{}, invalid("name the task: the message has no words to take a title from")
	}
	open, err := p.access.OpenToProject(ctx, conv)
	if err != nil {
		return Promoted{}, err
	}
	by, ok := authorOf(caller)
	if !ok {
		by = System
	}
	var plan promotionPlan
	if open {
		plan, err = p.inPlace(ctx, q, conv, msg, by)
	} else {
		plan = p.apart(ctx, conv, msg, by)
	}
	if err != nil {
		return Promoted{}, err
	}
	task, err := p.tasks.Create(ctx, process.CreateParams{
		ProjectID: project, Kind: in.Kind, Title: title, Description: plan.description, InTx: plan.inTx,
	}, actor)
	if isUniqueViolation(err) {
		return Promoted{}, conflict("somebody opened this thread or made it a task a moment ago; try again")
	}
	if err != nil {
		return Promoted{}, err
	}
	thread, err := p.threads.ForTask(ctx, task)
	if err != nil {
		return Promoted{}, err
	}
	return Promoted{Task: task, Conversation: thread.ID}, nil
}

// promotionPlan is what a promotion writes: the task's description, and what it links inside
// the transaction that creates the task.
type promotionPlan struct {
	description string
	inTx        func(q *db.Queries, task db.Task) error
}

// inPlace is a promotion where the whole project may read the conversation: the thread under the
// message (or the one it is a reply in) becomes the task's thread, and the description quotes it.
func (p *Promoter) inPlace(ctx context.Context, q *db.Queries, conv db.Conversation, msg db.Message, by Author) (promotionPlan, error) {
	thread, err := p.threadOf(ctx, q, conv, msg)
	if err != nil {
		return promotionPlan{}, err
	}
	var replies []db.Message
	if thread.ID.Valid {
		if replies, err = q.ListMessagesAfter(ctx, db.ListMessagesAfterParams{ConversationID: thread.ID, AfterSeq: 0, MaxRows: maxQuotedReplies}); err != nil {
			return promotionPlan{}, err
		}
	}
	return promotionPlan{
		description: promotionDescription(msg, replies, MessageLink(msg)),
		inTx: func(q *db.Queries, task db.Task) error {
			_, err := link(ctx, q, by, msg, thread, task)
			return err
		},
	}, nil
}

// apart is a promotion out of a private channel or a DM. The task belongs to the whole project,
// so it gets a thread of its own under its card in the task channel (opened like any other
// task's); nothing said privately goes into it but the promoter's own message, and the private
// conversation gets a line pointing at the task.
func (p *Promoter) apart(ctx context.Context, conv db.Conversation, msg db.Message, by Author) promotionPlan {
	return promotionPlan{
		description: privateDescription(msg, by),
		inTx: func(q *db.Queries, task db.Task) error {
			body := fmt.Sprintf("Made into a task: “%s” (gator://tasks/%s)", task.Title, uuidString(task.ID))
			_, err := postSystemText(ctx, q, conv, body)
			return err
		},
	}
}

// project is where the task goes: the conversation's project, or for a DM outside every project
// the one the caller names. The caller must be able to write there.
func (p *Promoter) project(ctx context.Context, caller auth.Principal, conv db.Conversation, asked pgtype.UUID) (pgtype.UUID, error) {
	project := conv.ProjectID
	switch {
	case !project.Valid && !asked.Valid:
		return project, invalid("this conversation is in no project; say which project the task goes to")
	case !project.Valid:
		project = asked
	case asked.Valid && asked != project:
		return project, invalid("a task made here goes to this conversation's project")
	}
	role, err := p.roles.ProjectRole(ctx, caller, project)
	if err != nil {
		return project, err
	}
	if role < auth.RoleMember {
		return project, ErrForbidden
	}
	return project, nil
}

// threadOf is the thread that becomes the task's: the one under the message, or the one the
// message is a reply in. None yet is a zero conversation; a thread that already is a task's
// cannot become another's.
func (p *Promoter) threadOf(ctx context.Context, q *db.Queries, conv db.Conversation, msg db.Message) (db.Conversation, error) {
	if conv.Kind == KindTask {
		return db.Conversation{}, conflict("this is already a task's conversation")
	}
	if conv.Kind == KindThread {
		return conv, nil
	}
	thread, err := q.GetThreadOfMessage(ctx, msg.ID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return db.Conversation{}, nil
	case err != nil:
		return db.Conversation{}, err
	case thread.Kind == KindTask:
		return db.Conversation{}, conflict("this message is already a task")
	}
	return thread, nil
}

// link makes the thread the task's, or opens the task's thread under the message, inside the
// transaction that creates the task.
func link(ctx context.Context, q *db.Queries, by Author, msg db.Message, thread db.Conversation, task db.Task) (db.Conversation, error) {
	if thread.ID.Valid {
		linked, err := q.LinkTaskConversation(ctx, db.LinkTaskConversationParams{ID: thread.ID, TaskID: task.ID, ProjectID: task.ProjectID})
		if errors.Is(err, pgx.ErrNoRows) {
			return linked, conflict("this thread became a task a moment ago")
		}
		if err != nil {
			return linked, err
		}
		return linked, emit(ctx, q, EventConversationUpdated, linked.ID, map[string]any{"kind": linked.Kind, "taskId": uuidString(task.ID)})
	}
	opened, err := q.CreateConversation(ctx, db.CreateConversationParams{
		ProjectID: task.ProjectID, Kind: KindTask, Visibility: VisibilityProject, ParentMessageID: msg.ID, TaskID: task.ID,
		CreatedByKind: by.Kind, CreatedBy: by.ID,
	})
	if err != nil {
		return opened, err
	}
	if msg.AuthorKind == AuthorUser && msg.AuthorID.Valid {
		if _, err := q.AddConversationMember(ctx, db.AddConversationMemberParams{
			ConversationID: opened.ID, MemberKind: MemberUser, MemberID: msg.AuthorID, Role: RoleMember, Notify: NotifyMentions,
		}); err != nil {
			return opened, err
		}
	}
	return opened, emit(ctx, q, EventConversationCreated, opened.ID, map[string]any{
		"kind": opened.Kind, "taskId": uuidString(task.ID), "parentMessageId": uuidString(msg.ID), "projectId": uuidString(task.ProjectID),
	})
}

// firstLine is the start of a message, as a title: its first non-empty line without Markdown
// markers, clipped.
func firstLine(body string, max int) string {
	for _, line := range strings.Split(body, "\n") {
		if line = stripMarkers(line); line != "" {
			return clip(line, max)
		}
	}
	return ""
}

// stripMarkers takes heading, quote and list markers off the start of a line.
func stripMarkers(line string) string {
	for {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "#"):
			t = strings.TrimLeft(t, "#")
		case strings.HasPrefix(t, ">"):
			t = t[1:]
		case strings.HasPrefix(t, "- "), strings.HasPrefix(t, "* "), strings.HasPrefix(t, "+ "):
			t = t[2:]
		default:
			return t
		}
		line = t
	}
}

// privateDescription is the description of a task made from a private conversation: the
// promoter's own message, quoted, and no one else's words.
func privateDescription(msg db.Message, by Author) string {
	if sameAuthor(msg, by) {
		return quote(clip(msg.Body, maxQuotedRunes)) + "\n\nMade from a message in a private conversation."
	}
	return "Made from a message in a private conversation; ask the people in it for the details."
}

// promotionDescription quotes the message and the first replies under it, and links back to it.
func promotionDescription(msg db.Message, replies []db.Message, link string) string {
	var b strings.Builder
	b.WriteString(quote(clip(msg.Body, maxQuotedRunes)))
	if len(replies) > 0 {
		b.WriteString("\n\nReplies:\n")
		for _, r := range replies {
			if r.DeletedAt.Valid || r.Kind != messageText || strings.TrimSpace(r.Body) == "" {
				continue
			}
			fmt.Fprintf(&b, "- %s\n", strings.ReplaceAll(clip(r.Body, maxReplyRunes), "\n", " "))
		}
	}
	fmt.Fprintf(&b, "\n\nFrom the conversation: %s", link)
	return strings.TrimSpace(b.String())
}

// MessageLink is where a client opens a message in its conversation.
func MessageLink(m db.Message) string {
	return fmt.Sprintf("gator://conversations/%s?seq=%d", uuidString(m.ConversationID), m.Seq)
}

func quote(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight("> "+l, " ")
	}
	return strings.Join(lines, "\n")
}

func clip(s string, max int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:max])) + "…"
}
