package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store/db"
)

// directoryStore is what the directory reads and writes. *db.Queries is one.
type directoryStore interface {
	GetAgent(ctx context.Context, id pgtype.UUID) (db.Agent, error)
	ListAgentsVisibleIn(ctx context.Context, projectID pgtype.UUID) ([]db.Agent, error)
	ListWorkspaceAgents(ctx context.Context) ([]db.Agent, error)
	CreateAgent(ctx context.Context, arg db.CreateAgentParams) (db.Agent, error)
	UpdateAgent(ctx context.Context, arg db.UpdateAgentParams) (db.Agent, error)
	DeleteAgent(ctx context.Context, id pgtype.UUID) (int64, error)
	CountActiveTurnsOfAgent(ctx context.Context, agentID pgtype.UUID) (int32, error)
	AgentStats(ctx context.Context, arg db.AgentStatsParams) (db.AgentStatsRow, error)
	ListAgentTurns(ctx context.Context, arg db.ListAgentTurnsParams) ([]db.AgentTurn, error)
}

// Directory keeps a project's agents: who sees them, who changes them. Everyone who may read a
// project sees its agents and the workspace's, to mention them; a project's admin makes and
// changes its own, a workspace admin the workspace's. A built-in agent is paused, never deleted,
// and keeps its handle and role, which the process and people's habits rely on.
type Directory struct {
	store  directoryStore
	roles  roleResolver
	rights conversationRights
	now    func() time.Time
}

// NewDirectory builds it.
func NewDirectory(store directoryStore, roles roleResolver, rights conversationRights) *Directory {
	return &Directory{store: store, roles: roles, rights: rights, now: time.Now}
}

// Draft is a new agent.
type Draft struct {
	Handle, DisplayName, Description, Avatar, Instructions, Tier string
	RoleKey                                                      *string
	Policy                                                       *Policy
}

// Patch names what changes; nil leaves a field as it is.
type Patch struct {
	Handle, DisplayName, Description, Avatar, Instructions, Tier, Status, RoleKey *string
	Policy                                                                        *Policy
}

// Stats is an agent's record over a window.
type Stats struct {
	Since                             time.Time
	Turns, Refused, Jobs, Done, Ended int
	CostUSD                           float64
}

// DoneShare is how much of the work that ended ended done; 0 with nothing ended.
func (s Stats) DoneShare() float64 {
	if s.Ended == 0 {
		return 0
	}
	return float64(s.Done) / float64(s.Ended)
}

var handlePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,31}$`)

// List answers the agents people of a project can mention: its own and the workspace's.
func (d *Directory) List(ctx context.Context, p auth.Principal, project pgtype.UUID) ([]db.Agent, error) {
	if err := d.require(ctx, p, project, auth.RoleViewer); err != nil {
		return nil, err
	}
	return d.store.ListAgentsVisibleIn(ctx, project)
}

// ListWorkspace answers the workspace's own agents, to anyone signed in.
func (d *Directory) ListWorkspace(ctx context.Context, p auth.Principal) ([]db.Agent, error) {
	if p.Kind != auth.KindUser {
		return nil, ErrForbidden
	}
	return d.store.ListWorkspaceAgents(ctx)
}

// Get answers an agent the caller may see.
func (d *Directory) Get(ctx context.Context, p auth.Principal, id pgtype.UUID) (db.Agent, error) {
	a, err := d.store.GetAgent(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && a.DeletedAt.Valid) {
		return db.Agent{}, ErrNotFound
	}
	if err != nil {
		return db.Agent{}, err
	}
	if err := d.canSee(ctx, p, a); err != nil {
		return db.Agent{}, err
	}
	return a, nil
}

// Create makes a project's agent (project set) or the workspace's (project zero).
func (d *Directory) Create(ctx context.Context, p auth.Principal, project pgtype.UUID, in Draft) (db.Agent, error) {
	if err := d.canManage(ctx, p, project); err != nil {
		return db.Agent{}, err
	}
	if in.Tier == "" {
		in.Tier = TierChat
	}
	policy, err := policyJSON(in.Policy)
	if err != nil {
		return db.Agent{}, err
	}
	params := db.CreateAgentParams{ProjectID: project, Handle: strings.TrimPrefix(strings.TrimSpace(in.Handle), "@"),
		DisplayName: strings.TrimSpace(in.DisplayName), Description: strings.TrimSpace(in.Description), Avatar: strings.TrimSpace(in.Avatar),
		RoleKey: roleKey(in.RoleKey), Instructions: in.Instructions, Tier: in.Tier, Policy: policy, Status: StatusActive}
	if p.Kind == auth.KindUser {
		params.CreatedBy = p.UserID
	}
	if err := validate(params.Handle, params.DisplayName, params.Description, params.Tier, params.Status); err != nil {
		return db.Agent{}, err
	}
	a, err := d.store.CreateAgent(ctx, params)
	if isUniqueViolation(err) {
		return db.Agent{}, fmt.Errorf("%w: @%s is taken where people would mention it", ErrConflict, params.Handle)
	}
	return a, err
}

// Update changes an agent. A built-in one keeps its handle and its role.
func (d *Directory) Update(ctx context.Context, p auth.Principal, id pgtype.UUID, in Patch) (db.Agent, error) {
	a, err := d.Get(ctx, p, id)
	if err != nil {
		return db.Agent{}, err
	}
	if err := d.canManage(ctx, p, a.ProjectID); err != nil {
		return db.Agent{}, err
	}
	builtin := sourceOf(a) == SourceBuiltin
	if builtin && ((in.Handle != nil && *in.Handle != a.Handle) || (in.RoleKey != nil && (a.RoleKey == nil || *in.RoleKey != *a.RoleKey))) {
		return db.Agent{}, fmt.Errorf("%w: a built-in agent keeps its handle and its role; make one of your own for another", ErrInvalid)
	}
	next := db.UpdateAgentParams{ID: a.ID, Handle: a.Handle, DisplayName: a.DisplayName, Description: a.Description, Avatar: a.Avatar,
		RoleKey: a.RoleKey, Instructions: a.Instructions, Tier: a.Tier, Policy: a.Policy, Status: a.Status}
	set := func(dst *string, src *string) {
		if src != nil {
			*dst = strings.TrimSpace(*src)
		}
	}
	set(&next.Handle, in.Handle)
	next.Handle = strings.TrimPrefix(next.Handle, "@")
	set(&next.DisplayName, in.DisplayName)
	set(&next.Description, in.Description)
	set(&next.Avatar, in.Avatar)
	set(&next.Tier, in.Tier)
	set(&next.Status, in.Status)
	if in.Instructions != nil {
		next.Instructions = *in.Instructions
	}
	if in.RoleKey != nil {
		next.RoleKey = roleKey(in.RoleKey)
	}
	if in.Policy != nil {
		if next.Policy, err = policyJSON(in.Policy); err != nil {
			return db.Agent{}, err
		}
	}
	if err := validate(next.Handle, next.DisplayName, next.Description, next.Tier, next.Status); err != nil {
		return db.Agent{}, err
	}
	out, err := d.store.UpdateAgent(ctx, next)
	if isUniqueViolation(err) {
		return db.Agent{}, fmt.Errorf("%w: @%s is taken where people would mention it", ErrConflict, next.Handle)
	}
	return out, err
}

// Delete puts away an agent a person made: it answers nobody from then on and its handle is free,
// while its turns stay as the record of what it did and cost. One still answering is not put
// away until it is done or stopped. A built-in agent is paused instead.
func (d *Directory) Delete(ctx context.Context, p auth.Principal, id pgtype.UUID) error {
	a, err := d.Get(ctx, p, id)
	if err != nil {
		return err
	}
	if err := d.canManage(ctx, p, a.ProjectID); err != nil {
		return err
	}
	switch sourceOf(a) {
	case SourceBuiltin:
		return fmt.Errorf("%w: a built-in agent is not deleted; pause it", ErrConflict)
	case SourceMarketplace:
		return fmt.Errorf("%w: @%s came from the marketplace; remove its installation instead", ErrConflict, a.Handle)
	}
	active, err := d.store.CountActiveTurnsOfAgent(ctx, a.ID)
	if err != nil {
		return err
	}
	if active > 0 {
		return fmt.Errorf("%w: @%s is still answering; stop its turns or wait for them first", ErrConflict, a.Handle)
	}
	_, err = d.store.DeleteAgent(ctx, a.ID)
	return err
}

// statsWindow is the record a profile shows.
const statsWindow = 30 * 24 * time.Hour

// Stats is the agent's record over the last 30 days. A workspace agent works in every project,
// so its record is for workspace admins: anyone else would read what other projects spent.
func (d *Directory) Stats(ctx context.Context, p auth.Principal, id pgtype.UUID) (db.Agent, Stats, error) {
	a, err := d.Get(ctx, p, id)
	if err != nil {
		return db.Agent{}, Stats{}, err
	}
	since := d.now().Add(-statsWindow)
	if !a.ProjectID.Valid && p.WorkspaceRole != "admin" {
		return a, Stats{Since: since}, nil
	}
	row, err := d.store.AgentStats(ctx, db.AgentStatsParams{AgentID: a.ID, Since: pgtype.Timestamptz{Time: since, Valid: true}})
	if err != nil {
		return db.Agent{}, Stats{}, err
	}
	return a, Stats{Since: since, Turns: int(row.Turns), Refused: int(row.Refused), Jobs: int(row.Jobs), Done: int(row.Done),
		Ended: int(row.Finished), CostUSD: row.CostUsd}, nil
}

// maxTurns bounds one page of an agent's history.
const maxTurns = 200

// Turns is the agent's history since a moment, newest first: the turns in conversations the
// caller may read.
func (d *Directory) Turns(ctx context.Context, p auth.Principal, id pgtype.UUID, since time.Time, limit int) ([]db.AgentTurn, error) {
	a, err := d.Get(ctx, p, id)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > maxTurns {
		limit = maxTurns
	}
	rows, err := d.store.ListAgentTurns(ctx, db.ListAgentTurnsParams{AgentID: a.ID, Since: pgtype.Timestamptz{Time: since, Valid: true}, MaxRows: int32(limit)})
	if err != nil {
		return nil, err
	}
	out := rows[:0]
	readable := map[[16]byte]bool{}
	for _, t := range rows {
		ok, seen := readable[t.ConversationID.Bytes]
		if !seen {
			_, r, err := d.rights.RightsByID(ctx, t.ConversationID, p)
			if err != nil {
				return nil, err
			}
			ok = r.Read
			readable[t.ConversationID.Bytes] = ok
		}
		if ok {
			out = append(out, t)
		}
	}
	return out, nil
}

// canSee: a project's agent for those who may read the project, the workspace's for everyone.
func (d *Directory) canSee(ctx context.Context, p auth.Principal, a db.Agent) error {
	if !a.ProjectID.Valid {
		if p.Kind != auth.KindUser {
			return ErrNotFound
		}
		return nil
	}
	if err := d.require(ctx, p, a.ProjectID, auth.RoleViewer); err != nil {
		return ErrNotFound
	}
	return nil
}

// canManage: a project's admin for its agents, a workspace admin for the workspace's.
func (d *Directory) canManage(ctx context.Context, p auth.Principal, project pgtype.UUID) error {
	if !project.Valid {
		if p.Kind == auth.KindUser && p.WorkspaceRole == "admin" {
			return nil
		}
		return ErrForbidden
	}
	return d.require(ctx, p, project, auth.RoleAdmin)
}

func (d *Directory) require(ctx context.Context, p auth.Principal, project pgtype.UUID, min auth.Role) error {
	role, err := d.roles.ProjectRole(ctx, p, project)
	if err != nil {
		return err
	}
	if role < min {
		if role < auth.RoleViewer {
			return ErrNotFound
		}
		return ErrForbidden
	}
	return nil
}

// Check holds an agent made another way — installed from the marketplace — to the rules Create
// holds people's to.
func Check(handle, name, description, tier string) error {
	return validate(handle, name, description, tier, StatusActive)
}

func validate(handle, name, description, tier, status string) error {
	switch {
	case !handlePattern.MatchString(handle):
		return fmt.Errorf("%w: a handle is 2–32 of a–z, 0–9 and '-', not starting with '-'", ErrInvalid)
	case name == "" || utf8.RuneCountInString(name) > 80:
		return fmt.Errorf("%w: a name is 1–80 characters", ErrInvalid)
	case utf8.RuneCountInString(description) > 200:
		return fmt.Errorf("%w: a description is one line, at most 200 characters", ErrInvalid)
	case tier != TierChat && tier != TierWork:
		return fmt.Errorf("%w: the tier is chat or work", ErrInvalid)
	case status != StatusActive && status != StatusPaused:
		return fmt.Errorf("%w: the status is active or paused", ErrInvalid)
	}
	return nil
}

func policyJSON(p *Policy) ([]byte, error) {
	if p == nil {
		return []byte("{}"), nil
	}
	if p.MaxCostUSD < 0 || p.DailyBudgetUSD < 0 {
		return nil, fmt.Errorf("%w: a cost cap is not negative", ErrInvalid)
	}
	return json.Marshal(p)
}

func roleKey(s *string) *string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil
	}
	v := strings.TrimSpace(*s)
	return &v
}

// sourceOf is where an agent comes from.
func sourceOf(a db.Agent) string {
	var s struct {
		Kind string `json:"kind"`
	}
	_ = json.Unmarshal(a.Source, &s)
	return s.Kind
}

// SourceOf is where an agent comes from: builtin, custom or marketplace.
func SourceOf(a db.Agent) string { return sourceOf(a) }

func isUniqueViolation(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && pg.Code == "23505"
}
