package conversations

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

// askTasks opens and reopens the tasks behind conversations with agents. process.Service is one.
type askTasks interface {
	TaskCreator
	Reopen(ctx context.Context, taskID pgtype.UUID, actor process.Actor, reason string) (db.Task, error)
}

// Asks finds the task an agent's answer in a conversation belongs to. Every job needs a task, so
// a conversation about no task gets a hidden one (kind ask): a thread becomes that task's thread,
// and a DM keeps one ask, named in its external refs. A task's own thread answers its task.
type Asks struct {
	pool  *pgxpool.Pool
	tasks askTasks
}

// NewAsks builds the finder.
func NewAsks(pool *pgxpool.Pool, tasks askTasks) *Asks {
	return &Asks{pool: pool, tasks: tasks}
}

// askRef is the external ref a DM's ask carries.
const askRef = "conversation"

// reopenReason is what the history says when a closed ask is asked again.
const reopenReason = "asked again in its conversation"

// For answers the task of home, the conversation an agent answers in (a thread, a task's thread
// or a DM), opening an ask for it as by when there is none. trigger is the message that asked.
func (a *Asks) For(ctx context.Context, home db.Conversation, trigger db.Message, by process.Actor) (db.Task, error) {
	if !home.ProjectID.Valid {
		return db.Task{}, invalid("agents answer in a project's conversations")
	}
	q := db.New(a.pool)
	switch home.Kind {
	case KindTask:
		task, err := q.GetTask(ctx, home.TaskID)
		if err != nil {
			return db.Task{}, err
		}
		return a.open(ctx, task, by)
	case KindThread:
		return a.create(ctx, home, trigger, by, func(q *db.Queries, task db.Task) error {
			linked, err := q.LinkTaskConversation(ctx, db.LinkTaskConversationParams{ID: home.ID, TaskID: task.ID, ProjectID: task.ProjectID})
			if errors.Is(err, pgx.ErrNoRows) {
				return errAlreadyLinked
			}
			if err != nil {
				return err
			}
			return emit(ctx, q, EventConversationUpdated, linked.ID, map[string]any{"kind": linked.Kind, "taskId": uuidString(task.ID)})
		})
	case KindDM:
		task, err := q.FindTaskByExternalRef(ctx, db.FindTaskByExternalRefParams{ProjectID: home.ProjectID, Key: askRef, Value: uuidString(home.ID)})
		if err == nil {
			return a.open(ctx, task, by)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return db.Task{}, err
		}
		return a.create(ctx, home, trigger, by, nil)
	}
	return db.Task{}, invalid("an agent answers in a thread, a task or a DM, not in a %s", home.Kind)
}

// errAlreadyLinked is a thread another answer made an ask of a moment ago.
var errAlreadyLinked = errors.New("this thread became a task a moment ago")

func (a *Asks) create(ctx context.Context, home db.Conversation, trigger db.Message, by process.Actor, link func(*db.Queries, db.Task) error) (db.Task, error) {
	open, err := a.openToAll(ctx, home)
	if err != nil {
		return db.Task{}, err
	}
	// A task is the project's to read; words said in a private channel or a DM stay there, and
	// the ask only points at them.
	title, description := askTitle, "In a private conversation: "+MessageLink(trigger)
	if open {
		if t := firstLine(trigger.Body, maxTitleRunes); t != "" {
			title = t
		}
		description = quote(clip(trigger.Body, maxQuotedRunes)) + "\n\nFrom the conversation: " + MessageLink(trigger)
	}
	p := process.CreateParams{ProjectID: home.ProjectID, Kind: process.AskKind, Title: title, Hidden: true,
		Description: description, InTx: link}
	if home.Kind == KindDM {
		p.ExternalRefs = map[string]string{askRef: uuidString(home.ID)}
	}
	task, err := a.tasks.Create(ctx, p, by)
	if errors.Is(err, errAlreadyLinked) {
		conv, err := db.New(a.pool).GetConversation(ctx, home.ID)
		if err != nil {
			return db.Task{}, err
		}
		return a.For(ctx, conv, trigger, by)
	}
	return task, err
}

// askTitle is an ask's title when the words that asked may not be its title.
const askTitle = "A conversation with an agent"

// openToAll says whether home is open to the whole project.
func (a *Asks) openToAll(ctx context.Context, home db.Conversation) (bool, error) {
	root := home
	if home.ParentMessageID.Valid {
		var err error
		if root, err = db.New(a.pool).GetAccessRoot(ctx, home.ID); err != nil {
			return false, err
		}
	}
	return openToProject(root), nil
}

// open answers a task that is open, reopening a closed ask: asking again in its conversation is
// picking it up again. A closed task of the process stays closed and says so.
func (a *Asks) open(ctx context.Context, task db.Task, by process.Actor) (db.Task, error) {
	if !task.ClosedAt.Valid {
		return task, nil
	}
	if task.Kind != process.AskKind {
		return db.Task{}, conflict("this task is closed; reopen it to ask an agent about it")
	}
	return a.tasks.Reopen(ctx, task.ID, by, reopenReason)
}
