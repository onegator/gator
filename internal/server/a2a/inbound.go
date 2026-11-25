package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/agents"
	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/bridges"
	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/limits"
	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/plugin"
)

// bridge is where an A2A client's asks are said: a conversation the a2a provider connected, posted
// with the same rules a messaging plugin is held to. bridges.Service is one.
type bridge interface {
	FindOrCreate(ctx context.Context, b bridges.Bridge, p plugin.ConversationFindOrCreateParams) (plugin.ConversationFindOrCreateResult, error)
	Post(ctx context.Context, b bridges.Bridge, p plugin.MessagePostParams) (plugin.MessagePostResult, error)
}

// tokens mints and revokes A2A clients' tokens. auth.Tokens is one.
type tokens interface {
	IssueWith(ctx context.Context, q *db.Queries, p auth.IssueParams) (string, db.ApiToken, error)
	Revoke(ctx context.Context, id pgtype.UUID) error
}

// Inbound is the side where others call Gator's agents: the clients a project's admin let in, the
// Agent Cards of the agents it exposes, and the JSON-RPC endpoint.
type Inbound struct {
	pool   *pgxpool.Pool
	q      *db.Queries
	bridge bridge
	tokens tokens
	roles  roles
	// PublicURL is where clients reach this server, for the cards' url.
	PublicURL string
	// Wait is how long a blocking message/send waits for the answer before it answers the task as
	// it stands (the client asks again with tasks/get).
	Wait time.Duration
	Now  func() time.Time

	mu   sync.Mutex
	lims map[int32]*limits.Limiter // by a client's rate a minute
}

// InboundDeps are what the inbound side needs.
type InboundDeps struct {
	Pool      *pgxpool.Pool
	Bridge    bridge
	Tokens    tokens
	Roles     roles
	PublicURL string
}

// NewInbound builds it.
func NewInbound(d InboundDeps) *Inbound {
	return &Inbound{pool: d.Pool, q: db.New(d.Pool), bridge: d.Bridge, tokens: d.Tokens, roles: d.Roles,
		PublicURL: strings.TrimRight(d.PublicURL, "/"), Wait: 25 * time.Second, Now: time.Now, lims: map[int32]*limits.Limiter{}}
}

var clientName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

// NewClient is an A2A client to let in.
type NewClient struct {
	Name           string
	DailyBudgetUSD *float64
	PerMinute      *int
}

// Created is a client and its token, shown once.
type Created struct {
	Client db.A2aClient
	Token  string
}

// CreateClient lets an outside agent in: a token (gtr_a2a_…) that reaches only the A2A endpoint of
// this project, a daily budget and a rate. For the project's admins.
func (in *Inbound) CreateClient(ctx context.Context, p auth.Principal, project pgtype.UUID, c NewClient) (Created, error) {
	if err := in.admin(ctx, p, project); err != nil {
		return Created{}, err
	}
	if !clientName.MatchString(c.Name) {
		return Created{}, fmt.Errorf("%w: a client's name is 1–40 lowercase letters, digits and dashes, starting with a letter", ErrInvalid)
	}
	budget, rate := float64(defaultDailyBudget), defaultPerMinute
	if c.DailyBudgetUSD != nil {
		budget = *c.DailyBudgetUSD
	}
	if c.PerMinute != nil {
		rate = *c.PerMinute
	}
	if budget < 0 || rate < 1 || rate > 600 {
		return Created{}, fmt.Errorf("%w: the budget is not negative and the rate is 1–600 a minute", ErrInvalid)
	}
	var out Created
	err := store.InTx(ctx, in.pool, func(q *db.Queries) error {
		ident, err := q.UpsertExternalIdentity(ctx, db.UpsertExternalIdentityParams{ProjectID: project, Provider: providerA2A,
			ExternalID: "client/" + c.Name, DisplayName: c.Name})
		if err != nil {
			return err
		}
		plaintext, tok, err := in.tokens.IssueWith(ctx, q, auth.IssueParams{Kind: auth.KindA2A, Scope: "a2a:" + c.Name,
			Name: "a2a " + c.Name, ProjectID: project})
		if err != nil {
			return err
		}
		client, err := q.CreateA2AClient(ctx, db.CreateA2AClientParams{ProjectID: project, Name: c.Name, TokenID: tok.ID,
			IdentityID: ident.ID, DailyBudgetUsd: budget, PerMinute: int32(rate), CreatedBy: p.UserID})
		if err != nil {
			return err
		}
		out = Created{Client: client, Token: plaintext}
		return nil
	})
	if pe, ok := errors.AsType[*pgconn.PgError](err); ok && pe.Code == "23505" {
		return Created{}, fmt.Errorf("%w: this project already has a client called %s", ErrConflict, c.Name)
	}
	return out, err
}

func (in *Inbound) admin(ctx context.Context, p auth.Principal, project pgtype.UUID) error {
	role, err := in.roles.ProjectRole(ctx, p, project)
	if err != nil {
		return err
	}
	if role < auth.RoleViewer {
		return ErrNotFound
	}
	if role < auth.RoleAdmin || p.Kind != auth.KindUser {
		return ErrForbidden
	}
	return nil
}

// ClientView is a client with what it spent today.
type ClientView struct {
	Client     db.A2aClient
	SpentToday float64
}

// Clients are a project's A2A clients. For its admins.
func (in *Inbound) Clients(ctx context.Context, p auth.Principal, project pgtype.UUID) ([]ClientView, error) {
	if err := in.admin(ctx, p, project); err != nil {
		return nil, err
	}
	rows, err := in.q.ListA2AClients(ctx, project)
	if err != nil {
		return nil, err
	}
	out := make([]ClientView, 0, len(rows))
	for _, c := range rows {
		spent, err := in.spent(ctx, c)
		if err != nil {
			return nil, err
		}
		out = append(out, ClientView{Client: c, SpentToday: spent})
	}
	return out, nil
}

// Revoke takes a client's token away; its asks stay in the conversation.
func (in *Inbound) Revoke(ctx context.Context, p auth.Principal, id pgtype.UUID) (db.A2aClient, error) {
	c, err := in.q.GetA2AClient(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.A2aClient{}, ErrNotFound
	}
	if err != nil {
		return db.A2aClient{}, err
	}
	if err := in.admin(ctx, p, c.ProjectID); err != nil {
		return db.A2aClient{}, err
	}
	if err := in.tokens.Revoke(ctx, c.TokenID); err != nil {
		return db.A2aClient{}, err
	}
	return in.q.RevokeA2AClient(ctx, id)
}

// spent is what a client's asks cost today (UTC), jobs still running counted at their cap.
func (in *Inbound) spent(ctx context.Context, c db.A2aClient) (float64, error) {
	day := in.Now().UTC().Truncate(24 * time.Hour)
	return in.q.ExternalCostSince(ctx, db.ExternalCostSinceParams{IdentityID: c.IdentityID, Since: pgtype.Timestamptz{Time: day, Valid: true}})
}

// SetExposed shows an agent of the project to its A2A clients, or stops. For the project's admins.
func (in *Inbound) SetExposed(ctx context.Context, p auth.Principal, agent db.Agent, exposed bool) (db.Agent, error) {
	if !agent.ProjectID.Valid {
		return db.Agent{}, fmt.Errorf("%w: only a project's own agent is shown to its clients", ErrInvalid)
	}
	if err := in.admin(ctx, p, agent.ProjectID); err != nil {
		return db.Agent{}, err
	}
	a, err := in.q.SetAgentExposed(ctx, db.SetAgentExposedParams{ID: agent.ID, Exposed: exposed})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Agent{}, fmt.Errorf("%w: a remote agent is not shown to clients", ErrInvalid)
	}
	return a, err
}

// Handler serves /a2a/{project}/{handle}: the agent's card (anyone may read the card of an agent a
// project exposes) and its JSON-RPC endpoint (an A2A client of that project only).
func (in *Inbound) Handler() http.Handler {
	r := chi.NewRouter()
	// Anyone may read an exposed agent's card, at a person's pace.
	cards := limits.NewLimiter(5, 20)
	r.With(limits.Middleware(cards, limits.ByBearerOrIP, nil)).Get("/{project}/{handle}"+CardPath, in.card)
	r.Post("/{project}/{handle}", in.rpc)
	return r
}

// AgentURL is where an exposed agent is called.
func (in *Inbound) AgentURL(slug, handle string) string {
	return in.PublicURL + "/a2a/" + slug + "/" + handle
}

func (in *Inbound) card(w http.ResponseWriter, r *http.Request) {
	slug, handle := chi.URLParam(r, "project"), chi.URLParam(r, "handle")
	a, err := in.q.GetExposedAgent(r.Context(), db.GetExposedAgentParams{Slug: slug, Handle: handle})
	if err != nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, in.cardOf(slug, a))
}

func (in *Inbound) cardOf(slug string, a db.Agent) AgentCard {
	desc := a.Description
	if desc == "" {
		desc = "@" + a.Handle + ", an agent of a Gator project."
	}
	skill := Skill{ID: a.Handle, Name: a.DisplayName, Description: desc}
	if a.RoleKey != nil {
		skill.Tags = []string{*a.RoleKey}
	}
	return AgentCard{ProtocolVersion: ProtocolVersion, Name: a.DisplayName, Description: desc, URL: in.AgentURL(slug, a.Handle),
		PreferredTransport: jsonRPCTransportName, Version: "1", DefaultInputModes: []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"}, Skills: []Skill{skill},
		SecuritySchemes: map[string]SecurityScheme{bearerSchemeName: {Type: "http", Scheme: "bearer"}},
		Security:        []map[string][]string{{bearerSchemeName: {}}}}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// rpcCall is one authenticated call: the client, the agent it asks, in the client's project.
type rpcCall struct {
	client db.A2aClient
	agent  db.Agent
	slug   string
}

func (in *Inbound) rpc(w http.ResponseWriter, r *http.Request) {
	reply := func(id json.RawMessage, result any, e *RPCError) {
		resp := rpcResponse{JSONRPC: "2.0", ID: id, Error: e}
		if e == nil {
			resp.Result, _ = json.Marshal(result)
		}
		if len(resp.ID) == 0 {
			resp.ID = json.RawMessage("null")
		}
		writeJSON(w, http.StatusOK, resp)
	}
	p, ok := auth.FromContext(r.Context())
	if !ok || p.Kind != auth.KindA2A {
		w.Header().Set("WWW-Authenticate", `Bearer realm="gator-a2a"`)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "an A2A client's token is required"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		reply(nil, nil, &RPCError{Code: codeInvalidRequest, Message: "the request is too large or unreadable"})
		return
	}
	var req rpcRequest
	if json.Unmarshal(body, &req) != nil || req.JSONRPC != "2.0" || req.Method == "" {
		reply(nil, nil, &RPCError{Code: codeParse, Message: "not a JSON-RPC 2.0 request"})
		return
	}
	call, e := in.authorize(r.Context(), p, chi.URLParam(r, "project"), chi.URLParam(r, "handle"))
	if e != nil {
		reply(req.ID, nil, e)
		return
	}
	var result any
	switch req.Method {
	case methodSend:
		result, e = in.send(r.Context(), call, req.Params)
	case methodGetTask:
		result, e = in.get(r.Context(), call, req.Params)
	case methodCancelTask:
		e = &RPCError{Code: codeNotCancelable, Message: "a task is stopped by a person in Gator"}
	case methodStream, methodResubscribe:
		e = &RPCError{Code: codeUnsupported, Message: "this agent does not stream; ask with message/send and tasks/get"}
	default:
		e = &RPCError{Code: codeMethodNotFound, Message: "method " + req.Method + " not found"}
	}
	reply(req.ID, result, e)
}

// authorize finds the caller's client and the agent it calls, which must be an exposed agent of
// the client's own project; anything else is an agent that does not exist.
func (in *Inbound) authorize(ctx context.Context, p auth.Principal, slug, handle string) (rpcCall, *RPCError) {
	notFound := &RPCError{Code: codeInvalidParams, Message: "no such agent"}
	client, err := in.q.GetA2AClientByToken(ctx, p.TokenID)
	if err != nil {
		return rpcCall{}, notFound
	}
	agent, err := in.q.GetExposedAgent(ctx, db.GetExposedAgentParams{Slug: slug, Handle: handle})
	if err != nil || agent.ProjectID != client.ProjectID || p.ProjectID != client.ProjectID {
		return rpcCall{}, notFound
	}
	return rpcCall{client: client, agent: agent, slug: slug}, nil
}

// allow is the client's rate: per_minute requests a minute, in bursts of as many.
func (in *Inbound) allow(c db.A2aClient) bool {
	in.mu.Lock()
	l, ok := in.lims[c.PerMinute]
	if !ok {
		l = limits.NewLimiter(float64(c.PerMinute)/60, int(c.PerMinute))
		in.lims[c.PerMinute] = l
	}
	in.mu.Unlock()
	return l.Allow(conversations.UUIDString(c.ID))
}

// send is message/send: the ask becomes a message of the client's in #a2a-<client>, asking the
// agent, and the client gets the task that answers it.
func (in *Inbound) send(ctx context.Context, c rpcCall, raw json.RawMessage) (any, *RPCError) {
	if !in.allow(c.client) {
		return nil, &RPCError{Code: codeRateLimited, Message: fmt.Sprintf("at most %d requests a minute", c.client.PerMinute)}
	}
	var p SendParams
	if json.Unmarshal(raw, &p) != nil {
		return nil, &RPCError{Code: codeInvalidParams, Message: "params are a message"}
	}
	for _, part := range p.Message.Parts {
		if part.Kind != "text" {
			return nil, &RPCError{Code: codeContentType, Message: "this agent reads text parts only"}
		}
	}
	text := p.Message.Text()
	switch {
	case p.Message.MessageID == "" || len(p.Message.MessageID) > 200 || strings.ContainsAny(p.Message.MessageID, "\n\r"):
		return nil, &RPCError{Code: codeInvalidParams, Message: "the message needs a messageId of at most 200 characters"}
	case text == "":
		return nil, &RPCError{Code: codeInvalidParams, Message: "the message says nothing"}
	case utf8.RuneCountInString(text) > maxTextRunes:
		return nil, &RPCError{Code: codeInvalidParams, Message: fmt.Sprintf("a message is at most %d characters", maxTextRunes)}
	}
	spent, err := in.spent(ctx, c.client)
	if err != nil {
		return nil, internal(err)
	}
	if spent >= c.client.DailyBudgetUsd {
		return nil, &RPCError{Code: codeBudget, Message: fmt.Sprintf("this client spent $%.2f of its $%.2f today", spent, c.client.DailyBudgetUsd)}
	}
	b := bridges.Bridge{Project: c.client.ProjectID, Provider: providerA2A}
	conv, err := in.bridge.FindOrCreate(ctx, b, plugin.ConversationFindOrCreateParams{ExternalRef: "client/" + c.client.Name,
		Kind: plugin.BridgeChannel, Name: channelPrefix + c.client.Name, Topic: "What the A2A client " + c.client.Name + " asked"})
	if err != nil {
		return nil, internal(err)
	}
	posted, err := in.bridge.Post(ctx, b, plugin.MessagePostParams{ConversationID: conv.ConversationID, Body: text,
		Author:      plugin.ExternalAuthor{ExternalID: "client/" + c.client.Name, DisplayName: c.client.Name},
		ExternalRef: "client/" + c.client.Name + "/" + p.Message.MessageID, Mentions: []string{c.agent.Handle}})
	if err != nil {
		return nil, internal(err)
	}
	msg, _ := parseUUID(posted.MessageID)
	task, err := in.q.CreateA2ATask(ctx, db.CreateA2ATaskParams{ClientID: c.client.ID, ProjectID: c.client.ProjectID, AgentID: c.agent.ID,
		MessageID: msg, ContextID: oneLine(p.Message.ContextID, 200)})
	if err != nil {
		return nil, internal(err)
	}
	view, err := in.view(ctx, task)
	if err != nil {
		return nil, internal(err)
	}
	if p.Configuration != nil && p.Configuration.Blocking {
		deadline := in.Now().Add(in.Wait)
		for !final(view.Status.State) && in.Now().Before(deadline) {
			select {
			case <-ctx.Done():
				return view, nil
			case <-time.After(250 * time.Millisecond):
			}
			if view, err = in.view(ctx, task); err != nil {
				return nil, internal(err)
			}
		}
	}
	return view, nil
}

// get is tasks/get, for a task the client was given.
func (in *Inbound) get(ctx context.Context, c rpcCall, raw json.RawMessage) (any, *RPCError) {
	var p TaskQuery
	id, ok := pgtype.UUID{}, false
	if json.Unmarshal(raw, &p) == nil {
		id, ok = parseUUID(p.ID)
	}
	missing := &RPCError{Code: codeTaskNotFound, Message: "task not found"}
	if !ok {
		return nil, missing
	}
	task, err := in.q.GetA2ATask(ctx, id)
	if err != nil || task.ClientID != c.client.ID || task.AgentID != c.agent.ID {
		return nil, missing
	}
	view, err := in.view(ctx, task)
	if err != nil {
		return nil, internal(err)
	}
	return view, nil
}

// view is a client's task as A2A says it: the turn's state, and the agent's answer once written.
func (in *Inbound) view(ctx context.Context, t db.A2aTask) (Task, error) {
	out := Task{Kind: "task", ID: conversations.UUIDString(t.ID), ContextID: t.ContextID,
		Status: TaskStatus{State: StateSubmitted}, Metadata: map[string]any{}}
	if out.ContextID == "" {
		out.ContextID = conversations.UUIDString(t.MessageID)
	}
	turn, err := in.q.GetTurnByTrigger(ctx, db.GetTurnByTriggerParams{AgentID: t.AgentID, TriggerMessageID: t.MessageID})
	if errors.Is(err, pgx.ErrNoRows) {
		return out, nil // the agents have not read it yet
	}
	if err != nil {
		return out, err
	}
	out.Metadata["turnId"] = conversations.UUIDString(turn.ID)
	if turn.JobID.Valid {
		if cost, err := in.q.JobCost(ctx, turn.JobID); err == nil {
			out.Metadata["costUsd"] = cost.CostUsd
		}
	}
	answer := ""
	if turn.ReplyMessageID.Valid {
		if m, err := in.q.GetMessage(ctx, turn.ReplyMessageID); err == nil && m.Status != conversations.StatusStreaming && !m.DeletedAt.Valid {
			answer = m.Body
		}
	}
	switch turn.Status {
	case agents.TurnQueued:
		out.Status.State = StateSubmitted
	case agents.TurnRunning:
		out.Status.State = StateWorking
	case agents.TurnDone:
		out.Status.State = StateCompleted
	case agents.TurnStopped:
		out.Status.State = StateCanceled
	case agents.TurnRefused:
		out.Status.State = StateRejected
	default:
		out.Status.State = StateFailed
	}
	if answer != "" && final(out.Status.State) {
		m := Message{Kind: "message", Role: "agent", MessageID: conversations.UUIDString(turn.ReplyMessageID), Parts: []Part{{Kind: "text", Text: answer}}}
		out.Status.Message = &m
		if out.Status.State == StateCompleted {
			out.Artifacts = []Artifact{{ArtifactID: conversations.UUIDString(turn.ReplyMessageID), Name: "answer", Parts: m.Parts}}
		}
	}
	return out, nil
}

func internal(err error) *RPCError {
	if pe, ok := errors.AsType[*plugin.Error](err); ok {
		return &RPCError{Code: codeInvalidParams, Message: pe.Message}
	}
	return &RPCError{Code: codeInternal, Message: "internal error"}
}

func parseUUID(s string) (pgtype.UUID, bool) {
	var u pgtype.UUID
	return u, u.Scan(s) == nil && u.Valid
}
