package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/agents"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/secrets"
	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/internal/server/words"
)

// Errors of the people's side.
var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("not allowed")
	ErrConflict  = errors.New("conflict")
	ErrNoKeyring = errors.New("a remote agent's credential needs GATOR_SECRETS_KEY")
)

// roles answers a person's role in a project. auth.Authorizer is one.
type roles interface {
	ProjectRole(ctx context.Context, p auth.Principal, project pgtype.UUID) (auth.Role, error)
}

// rewriter writes a remote agent's answer into its message. conversations.Voice is one.
type rewriter interface {
	Rewrite(ctx context.Context, message pgtype.UUID, body, status string) (db.Message, bool, error)
}

// opener answers whether a conversation is open to its whole project. conversations.Access is one.
type opener interface {
	OpenToProject(ctx context.Context, conv db.Conversation) (bool, error)
}

// packer tells what a conversation says. conversations.Packer is one.
type packer interface {
	Pack(ctx context.Context, conv db.Conversation, ask db.Message) (conversations.Pack, error)
}

// RemoteInfo is what Gator keeps of a remote agent's card (agents.remote).
type RemoteInfo struct {
	CardURL     string    `json:"card_url"`
	URL         string    `json:"url"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Version     string    `json:"version,omitempty"`
	Skills      []Skill   `json:"skills,omitempty"`
	FetchedAt   time.Time `json:"fetched_at"`
}

// InfoOf reads a remote agent's card as kept.
func InfoOf(a db.Agent) RemoteInfo {
	var r RemoteInfo
	_ = json.Unmarshal(a.Remote, &r)
	return r
}

// Remote is the outbound side: remote agents and the calls that answer their turns.
type Remote struct {
	q      remoteStore
	keys   *secrets.Keyring
	client Client
	roles  roles
	voice  rewriter
	open   opener
	pack   packer
	log    *slog.Logger
	// CallTimeout bounds one call, and Patience how long a task that is still working is asked
	// about again before the turn ends without an answer.
	CallTimeout time.Duration
	Patience    time.Duration
	PollEvery   time.Duration
	Workers     int
	PerAgent    int
	DailyCalls  int
}

// RemoteDeps are what the outbound side needs.
type RemoteDeps struct {
	Queries remoteStore
	Keys    *secrets.Keyring
	Client  Client
	Roles   roles
	Voice   rewriter
	Open    opener
	Packer  packer
	Log     *slog.Logger
}

// NewRemote builds it.
func NewRemote(d RemoteDeps) *Remote {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	return &Remote{q: d.Queries, keys: d.Keys, client: d.Client, roles: d.Roles, voice: d.Voice, open: d.Open, pack: d.Packer,
		log: d.Log, CallTimeout: 2 * time.Minute, Patience: 10 * time.Minute, PollEvery: 2 * time.Second,
		Workers: DefaultWorkers, PerAgent: DefaultPerAgent, DailyCalls: DefaultDailyCalls}
}

// NewRemoteAgent is a remote agent to add to a project.
type NewRemoteAgent struct {
	Handle  string
	CardURL string
	// Token is the credential its endpoint takes (HTTP bearer); empty for none.
	Token string
	Name  string // default: the card's
}

// Create adds a remote agent to a project: its card is fetched (through the server's outbound
// policy) and checked, its credential encrypted. For the project's admins.
func (r *Remote) Create(ctx context.Context, p auth.Principal, project pgtype.UUID, in NewRemoteAgent) (db.Agent, error) {
	role, err := r.roles.ProjectRole(ctx, p, project)
	if err != nil {
		return db.Agent{}, err
	}
	if role < auth.RoleViewer {
		return db.Agent{}, ErrNotFound
	}
	if role < auth.RoleAdmin || p.Kind != auth.KindUser {
		return db.Agent{}, ErrForbidden
	}
	card, cardURL, err := r.client.FetchCard(ctx, in.CardURL)
	if err != nil {
		return db.Agent{}, err
	}
	// The credential goes where the card says the agent is; a card that sends it to another host
	// than its own is not taken.
	if c, e := url.Parse(cardURL); e != nil || !strings.EqualFold(c.Host, hostOf(card.URL)) {
		return db.Agent{}, fmt.Errorf("%w: the card's endpoint is on another host than the card", ErrInvalid)
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = words.OneLine(card.Name, 80)
	}
	desc := words.OneLine(card.Description, 200)
	if err := agents.Check(in.Handle, name, desc, agents.TierChat); err != nil {
		return db.Agent{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	info := RemoteInfo{CardURL: cardURL, URL: card.URL, Name: words.OneLine(card.Name, 80), Description: desc, Version: words.OneLine(card.Version, 40),
		FetchedAt: time.Now().UTC()}
	for i, s := range card.Skills {
		if i == 20 {
			break
		}
		info.Skills = append(info.Skills, Skill{ID: words.OneLine(s.ID, 64), Name: words.OneLine(s.Name, 80), Description: words.OneLine(s.Description, 200)})
	}
	remote, _ := json.Marshal(info)
	sealed := ""
	if in.Token != "" {
		if r.keys == nil {
			return db.Agent{}, ErrNoKeyring
		}
		if sealed, err = r.keys.Encrypt([]byte(in.Token)); err != nil {
			return db.Agent{}, err
		}
	}
	a, err := r.q.CreateRemoteAgent(ctx, db.CreateRemoteAgentParams{ProjectID: project, Handle: in.Handle, DisplayName: name,
		Description: desc, Remote: remote, RemoteSecrets: sealed, CreatedBy: p.UserID})
	if pe, ok := errors.AsType[*pgconn.PgError](err); ok && pe.Code == "23505" {
		return db.Agent{}, fmt.Errorf("%w: @%s is taken where people would mention it", ErrConflict, in.Handle)
	}
	return a, err
}

func hostOf(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	return u.Host
}

var _ agents.Elsewhere = (*Remote)(nil)

// Begin implements agents.Elsewhere: the turn of a remote agent becomes a call, made by the worker.
// A remote agent answers only where the whole project reads: what is said in a DM or a private
// channel does not leave for another system.
func (r *Remote) Begin(ctx context.Context, turn db.AgentTurn, req agents.Request, home db.Conversation) (*agents.Refusal, error) {
	open, err := r.open.OpenToProject(ctx, home)
	if err != nil {
		return nil, err
	}
	if !open {
		return &agents.Refusal{Say: fmt.Sprintf("@%s is an agent outside Gator; it answers only in conversations open to the whole project.", req.Agent.Handle)}, nil
	}
	if InfoOf(req.Agent).URL == "" {
		return &agents.Refusal{Say: fmt.Sprintf("@%s has no endpoint; a project admin adds it again.", req.Agent.Handle)}, nil
	}
	day := time.Now().UTC().Truncate(24 * time.Hour)
	n, err := r.q.CountA2ACallsSince(ctx, db.CountA2ACallsSinceParams{AgentID: req.Agent.ID, Since: pgtype.Timestamptz{Time: day, Valid: true}})
	if err != nil {
		return nil, err
	}
	if n >= int64(r.DailyCalls) {
		return &agents.Refusal{Code: agents.RefusedAgentBudget, Say: fmt.Sprintf("@%s does not answer today: it was asked %d times, its limit for a day.", req.Agent.Handle, n)}, nil
	}
	return nil, r.q.InsertA2ACall(ctx, db.InsertA2ACallParams{TurnID: turn.ID, AgentID: req.Agent.ID, ProjectID: turn.ProjectID})
}

// ClaimTTL is how long a worker may hold a call before another takes it over.
const ClaimTTL = 15 * time.Minute

// Limits of the calls out: how many go at once (Workers), to one agent at once (PerAgent), how
// often one call is tried before it fails (MaxAttempts), and how many one agent is given a day
// (DailyCalls) — a remote turn costs Gator nothing it can count, so its budget is in calls.
const (
	DefaultWorkers    = 4
	DefaultPerAgent   = 2
	MaxAttempts       = 3
	DefaultDailyCalls = 100
)

// errStopped is a call whose turn a person stopped.
var errStopped = errors.New("stopped")

// Run makes the calls of remote agents' turns, Workers at a time, until ctx ends.
func (r *Remote) Run(ctx context.Context, every time.Duration) {
	var wg sync.WaitGroup
	for range max(r.Workers, 1) {
		wg.Go(func() { r.work(ctx, every) })
	}
	wg.Wait()
}

func (r *Remote) work(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		for ctx.Err() == nil {
			ok, err := r.CallOne(ctx)
			if err != nil && ctx.Err() == nil {
				r.log.Warn("a2a call", "err", err)
			}
			if !ok {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// CallOne makes the oldest call nobody is making; ok is false when there was none. A call whose
// turn has ended (a person stopped it) is not made, and one tried MaxAttempts times fails.
func (r *Remote) CallOne(ctx context.Context) (bool, error) {
	call, err := r.q.ClaimA2ACall(ctx, db.ClaimA2ACallParams{ClaimTtl: pgtype.Interval{Microseconds: ClaimTTL.Microseconds(), Valid: true},
		PerAgent: int64(max(r.PerAgent, 1))})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if call.Attempts > MaxAttempts {
		return true, r.end(context.WithoutCancel(ctx), call, fmt.Sprintf("gave up after %d tries", MaxAttempts), StateFailed)
	}
	answer, state, err := r.ask(ctx, call)
	if errors.Is(err, errStopped) {
		_, err := r.q.EndOpenA2ACall(context.WithoutCancel(ctx), db.EndOpenA2ACallParams{TurnID: call.TurnID, Error: "stopped"})
		return true, err
	}
	if err != nil {
		answer, state = err.Error(), StateFailed
	}
	return true, r.end(context.WithoutCancel(ctx), call, answer, state)
}

// running answers the turn of a call while it is still being answered, else errStopped.
func (r *Remote) running(ctx context.Context, id pgtype.UUID) (db.AgentTurn, error) {
	turn, err := r.q.GetTurn(ctx, id)
	if err != nil {
		return turn, err
	}
	if turn.Status != agents.TurnRunning {
		return turn, errStopped
	}
	return turn, nil
}

// Stopped implements agents.Elsewhere: a person stopped the turn, and its call is not made, or not
// waited for, any more.
func (r *Remote) Stopped(ctx context.Context, turn db.AgentTurn) error {
	_, err := r.q.EndOpenA2ACall(ctx, db.EndOpenA2ACallParams{TurnID: turn.ID, Error: "stopped"})
	return err
}

// ask gives the remote agent its turn and waits for its answer.
func (r *Remote) ask(ctx context.Context, call db.A2aCall) (string, string, error) {
	turn, err := r.running(ctx, call.TurnID)
	if err != nil {
		return "", "", err // stopped before anything was sent
	}
	agent, err := r.q.GetAgent(ctx, call.AgentID)
	if err != nil {
		return "", "", err
	}
	info := InfoOf(agent)
	token := ""
	if agent.RemoteSecrets != "" {
		if r.keys == nil {
			return "", "", ErrNoKeyring
		}
		b, err := r.keys.Decrypt(agent.RemoteSecrets)
		if err != nil {
			return "", "", err
		}
		token = string(b)
	}
	text, err := r.question(ctx, turn)
	if err != nil {
		return "", "", err
	}
	cctx, cancel := context.WithTimeout(ctx, r.CallTimeout)
	defer cancel()
	var task Task
	if call.RemoteTaskID != "" {
		task, err = r.client.Get(cctx, info.URL, token, call.RemoteTaskID) // a call a dead worker began
	} else {
		task, err = r.client.Send(cctx, info.URL, token, Message{Kind: "message", Role: "user",
			MessageID: conversations.UUIDString(turn.ID), ContextID: conversations.UUIDString(turn.ConversationID),
			Parts: []Part{{Kind: "text", Text: text}}})
	}
	if err != nil {
		return "", "", err
	}
	if task.ID != "" && !final(task.Status.State) {
		if err := r.q.SetA2ARemoteTask(ctx, db.SetA2ARemoteTaskParams{TurnID: call.TurnID, RemoteTaskID: task.ID}); err != nil {
			return "", "", err
		}
	}
	deadline := time.Now().Add(r.Patience)
	for !final(task.Status.State) {
		if time.Now().After(deadline) {
			return "", "", fmt.Errorf("@%s did not finish in %s", agent.Handle, r.Patience)
		}
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-time.After(r.PollEvery):
		}
		if _, err := r.running(ctx, call.TurnID); err != nil {
			return "", "", err // stopped while the remote worked: not waited for any more
		}
		gctx, cancel := context.WithTimeout(ctx, r.CallTimeout)
		task, err = r.client.Get(gctx, info.URL, token, task.ID)
		cancel()
		if err != nil {
			return "", "", err
		}
	}
	return task.Answer(), task.Status.State, nil
}

// question is what the remote agent is told: who asks what, where, and the latest of the
// conversation — the conversation is open to the project, and the agent was added by its admin.
func (r *Remote) question(ctx context.Context, turn db.AgentTurn) (string, error) {
	ask, err := r.q.GetMessage(ctx, turn.TriggerMessageID)
	if err != nil {
		return "", err
	}
	conv, err := r.q.GetConversation(ctx, turn.ConversationID)
	if err != nil {
		return "", err
	}
	p, err := r.pack.Pack(ctx, conv, ask)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s asks, in %s:\n\n%s\n", p.Asker, p.Where, strings.TrimSpace(p.Ask.Body))
	recent := p.Recent
	if len(recent) > 10 {
		recent = recent[len(recent)-10:]
	}
	if len(recent) > 0 {
		b.WriteString("\nThe conversation so far (latest messages):\n")
		for _, s := range recent {
			fmt.Fprintf(&b, "\n%s: %s\n", s.Who, strings.TrimSpace(s.Body))
		}
	}
	return words.Clip(b.String(), 16000), nil
}

// end writes what came back into the turn's message and ends the turn.
func (r *Remote) end(ctx context.Context, call db.A2aCall, answer, state string) error {
	turn, err := r.running(ctx, call.TurnID)
	if errors.Is(err, errStopped) {
		_, err := r.q.EndOpenA2ACall(ctx, db.EndOpenA2ACallParams{TurnID: call.TurnID, Error: "stopped"})
		return err // a person stopped it: what came back is not written over the stop
	}
	if err != nil {
		return err
	}
	agent, err := r.q.GetAgent(ctx, call.AgentID)
	if err != nil {
		return err
	}
	answer = words.Clip(strings.TrimSpace(answer), maxTextRunes)
	status, msgStatus, callStatus, why := agents.TurnDone, conversations.StatusFinal, "done", ""
	switch state {
	case StateCompleted, StateInputRequired:
		if answer == "" {
			answer = fmt.Sprintf("@%s answered with nothing.", agent.Handle)
		}
	default:
		status, msgStatus, callStatus, why = agents.TurnFailed, conversations.StatusFailed, "failed", state
		answer = fmt.Sprintf("@%s could not answer (%s). %s", agent.Handle, state, answer)
	}
	if turn.ReplyMessageID.Valid {
		if _, _, err := r.voice.Rewrite(ctx, turn.ReplyMessageID, strings.TrimSpace(answer), msgStatus); err != nil {
			return err
		}
	}
	if _, err := r.q.SetTurnStatus(ctx, db.SetTurnStatusParams{ID: turn.ID, Status: status}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return r.q.EndA2ACall(ctx, db.EndA2ACallParams{TurnID: call.TurnID, Status: callStatus, Error: words.OneLine(why, 500)})
}
