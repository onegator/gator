package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/agents"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/secrets"
	"github.com/onegator/gator/internal/server/store/db"
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
	q      *db.Queries
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
}

// RemoteDeps are what the outbound side needs.
type RemoteDeps struct {
	Queries *db.Queries
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
		log: d.Log, CallTimeout: 2 * time.Minute, Patience: 10 * time.Minute, PollEvery: 2 * time.Second}
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
		name = oneLine(card.Name, 80)
	}
	desc := oneLine(card.Description, 200)
	if err := agents.Check(in.Handle, name, desc, agents.TierChat); err != nil {
		return db.Agent{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	info := RemoteInfo{CardURL: cardURL, URL: card.URL, Name: oneLine(card.Name, 80), Description: desc, Version: oneLine(card.Version, 40),
		FetchedAt: time.Now().UTC()}
	for i, s := range card.Skills {
		if i == 20 {
			break
		}
		info.Skills = append(info.Skills, Skill{ID: oneLine(s.ID, 64), Name: oneLine(s.Name, 80), Description: oneLine(s.Description, 200)})
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

// oneLine keeps one line of at most n characters of an outside text.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		s = string(r[:n])
	}
	return s
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
	return nil, r.q.InsertA2ACall(ctx, db.InsertA2ACallParams{TurnID: turn.ID, AgentID: req.Agent.ID, ProjectID: turn.ProjectID})
}

// ClaimTTL is how long a worker may hold a call before another takes it over.
const ClaimTTL = 15 * time.Minute

// Run makes the calls of remote agents' turns, one at a time, until ctx ends.
func (r *Remote) Run(ctx context.Context, every time.Duration) {
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

// CallOne makes the oldest call nobody is making; ok is false when there was none.
func (r *Remote) CallOne(ctx context.Context) (bool, error) {
	call, err := r.q.ClaimA2ACall(ctx, pgtype.Interval{Microseconds: ClaimTTL.Microseconds(), Valid: true})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	answer, state, err := r.ask(ctx, call)
	if err != nil {
		answer, state = err.Error(), StateFailed
	}
	return true, r.end(context.WithoutCancel(ctx), call, answer, state)
}

// ask gives the remote agent its turn and waits for its answer.
func (r *Remote) ask(ctx context.Context, call db.A2aCall) (string, string, error) {
	turn, err := r.q.GetTurn(ctx, call.TurnID)
	if err != nil {
		return "", "", err
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
	return clipRunes(b.String(), 16000), nil
}

// end writes what came back into the turn's message and ends the turn.
func (r *Remote) end(ctx context.Context, call db.A2aCall, answer, state string) error {
	turn, err := r.q.GetTurn(ctx, call.TurnID)
	if err != nil {
		return err
	}
	agent, err := r.q.GetAgent(ctx, call.AgentID)
	if err != nil {
		return err
	}
	answer = clipRunes(strings.TrimSpace(answer), maxTextRunes)
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
	return r.q.EndA2ACall(ctx, db.EndA2ACallParams{TurnID: call.TurnID, Status: callStatus, Error: oneLine(why, 500)})
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
