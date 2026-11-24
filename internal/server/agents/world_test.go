package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/runners"
	"github.com/onegator/gator/internal/server/store/db"
)

// world is a project with people, its #general, and the server's services over a real database.
// Events are handed to the dispatcher by the test, one at a time, instead of by a consumer.
type world struct {
	t       *testing.T
	ctx     context.Context
	pool    *pgxpool.Pool
	q       *db.Queries
	svc     *process.Service
	chat    *conversations.Module
	mgr     *runners.Manager
	mod     *Module
	admin   auth.Principal
	viewer  auth.Principal
	project pgtype.UUID
	general db.Conversation
}

func newWorld(t *testing.T) *world {
	t.Helper()
	pool := testPool(t)
	ctx := context.Background()
	cat, err := process.DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	svc := process.NewService(pool, process.StaticCatalog(cat), process.StaticCapabilities(nil))
	roles := auth.Authorizer{Pool: pool}
	chat := conversations.New(pool, conversations.Deps{Roles: roles, Decisions: svc, Tasks: svc})
	mgr := runners.New(pool, svc, events.NewHub(), slog.Default(), runners.Config{})
	w := &world{t: t, ctx: ctx, pool: pool, q: db.New(pool), svc: svc, chat: chat, mgr: mgr,
		mod: New(pool, Deps{Conversations: chat, Process: svc, Jobs: mgr, Roles: roles})}
	w.project = newProject(t, pool)
	w.admin = w.person("admin")
	w.viewer = w.person("viewer")
	general, err := chat.Directory.EnsureDefaultChannels(ctx, w.project, w.admin.UserID, w.viewer.UserID)
	if err != nil {
		t.Fatal(err)
	}
	w.general = general
	return w
}

func (w *world) person(role string) auth.Principal {
	w.t.Helper()
	var uid pgtype.UUID
	if err := w.pool.QueryRow(w.ctx, "INSERT INTO users(email,name,workspace_role) VALUES($1,$2,'member') RETURNING id",
		fmt.Sprintf("%s-%s@agents.test", role, unique()), role).Scan(&uid); err != nil {
		w.t.Fatal(err)
	}
	if _, err := w.pool.Exec(w.ctx, "INSERT INTO memberships(project_id,user_id,role) VALUES($1,$2,$3)", w.project, uid, role); err != nil {
		w.t.Fatal(err)
	}
	return auth.Principal{Kind: auth.KindUser, UserID: uid, WorkspaceRole: "member", Name: role}
}

func (w *world) agent(handle string) db.Agent {
	w.t.Helper()
	a, err := w.q.GetAgentByHandle(w.ctx, db.GetAgentByHandleParams{Handle: handle, ProjectID: w.project})
	if err != nil {
		w.t.Fatalf("@%s: %v", handle, err)
	}
	return a
}

func mentioning(agents ...db.Agent) []conversations.Member {
	out := make([]conversations.Member, 0, len(agents))
	for _, a := range agents {
		out = append(out, conversations.Member{Kind: conversations.MemberAgent, ID: a.ID})
	}
	return out
}

// post is a person writing in a conversation, then the dispatcher reading it.
func (w *world) post(p auth.Principal, conv pgtype.UUID, body string, agents ...db.Agent) db.Message {
	w.t.Helper()
	posted, err := w.chat.Messages.Post(w.ctx, p, conv, conversations.Draft{Body: body, Mentions: mentioning(agents...)})
	if err != nil {
		w.t.Fatal(err)
	}
	w.dispatch(posted.Message)
	return posted.Message
}

// delegate is an agent saying something in its turn's conversation, mentioning other agents.
func (w *world) delegate(from db.AgentTurn, body string, agents ...db.Agent) db.Message {
	w.t.Helper()
	conv, err := w.q.GetConversation(w.ctx, from.ConversationID)
	if err != nil {
		w.t.Fatal(err)
	}
	msg, err := w.mod.Voice.Say(w.ctx, conv, conversations.Speech{Agent: from.AgentID, Turn: from.ID, Body: body, Mentions: mentioning(agents...)})
	if err != nil {
		w.t.Fatal(err)
	}
	w.dispatch(msg)
	return msg
}

// dispatch hands the message.posted event of msg to the dispatcher, as the consumer would.
func (w *world) dispatch(msg db.Message) {
	w.t.Helper()
	payload, _ := json.Marshal(map[string]any{"messageId": conversations.UUIDString(msg.ID)})
	if err := w.mod.Dispatcher.Handle(w.ctx, db.EventsAfterRow{Type: conversations.EventMessagePosted, AggregateID: msg.ConversationID, Payload: payload}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) turnOf(a db.Agent, msg db.Message) db.AgentTurn {
	w.t.Helper()
	turn, err := w.q.GetTurnByTrigger(w.ctx, db.GetTurnByTriggerParams{AgentID: a.ID, TriggerMessageID: msg.ID})
	if err != nil {
		w.t.Fatalf("no turn of @%s for %q: %v", a.Handle, msg.Body, err)
	}
	return turn
}

func (w *world) count(query string, args ...any) int {
	w.t.Helper()
	var n int
	if err := w.pool.QueryRow(w.ctx, query, args...).Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// messagesIn are the messages of a conversation, oldest first.
func (w *world) messagesIn(conv pgtype.UUID) []db.Message {
	w.t.Helper()
	rows, err := w.q.ListMessagesAfter(w.ctx, db.ListMessagesAfterParams{ConversationID: conv, AfterSeq: 0, MaxRows: 100})
	if err != nil {
		w.t.Fatal(err)
	}
	return rows
}

func (w *world) policy(cfg string) {
	w.t.Helper()
	if _, err := w.pool.Exec(w.ctx, "UPDATE projects SET process_config = $2 WHERE id = $1", w.project, cfg); err != nil {
		w.t.Fatal(err)
	}
}

// spend records what a job cost, as a finished job's usage would.
func (w *world) spend(job pgtype.UUID, usd float64) {
	w.t.Helper()
	j, err := w.q.GetJob(w.ctx, job)
	if err != nil {
		w.t.Fatal(err)
	}
	if _, err := w.pool.Exec(w.ctx, `INSERT INTO usage_records (task_id, project_id, phase, job_id, source, cost_usd)
		VALUES ($1, $2, $3, $4, 'runner', $5)`, j.TaskID, j.ProjectID, j.Phase, j.ID, usd); err != nil {
		w.t.Fatal(err)
	}
}

// settled are the messages of a conversation once ok says they are as expected, or as they are
// after a while. Another test binary's server may be following the same outbox and answering the
// same event a moment earlier, so what it says can land just after this test asks.
func (w *world) settled(conv pgtype.UUID, ok func([]db.Message) bool) []db.Message {
	w.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		msgs := w.messagesIn(conv)
		if ok(msgs) || time.Now().After(deadline) {
			return msgs
		}
		time.Sleep(20 * time.Millisecond)
	}
}
