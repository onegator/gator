package process

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/brain"
	"github.com/onegator/gator/internal/server/store/db"
)

//go:embed roles/*.md
var rolesFS embed.FS

// DefaultRoleGuide returns the built-in prompt for a runner role, or "" if none exists.
func DefaultRoleGuide(role string) string {
	b, err := rolesFS.ReadFile("roles/" + role + ".md")
	if err != nil {
		return ""
	}
	return string(b)
}

// AutopilotConfig decides whether entering a runner phase queues a job by itself.
type AutopilotConfig struct {
	Enabled *bool  `json:"enabled"`
	Backend string `json:"backend"`
}

// ProjectConfig is the parsed shape of projects.process_config.
type ProjectConfig struct {
	Templates map[string]Template `json:"templates"`
	Roles     map[string]string   `json:"roles"` // role name → prompt that replaces the default
	Autopilot *AutopilotConfig    `json:"autopilot"`
	Policy    *Policy             `json:"policy"`
	// Quality is the project's scorecard: off unless it is asked for. Read by the quality
	// package, kept here so one document describes the whole process.
	Quality json.RawMessage `json:"quality,omitempty"`
	// Conversations says where the project's conversations put work, e.g. which channel a new
	// task's card goes to ({"task_channel": "work"}). Read by the conversations package.
	Conversations json.RawMessage `json:"conversations,omitempty"`
	// Checkpoints says whether runners checkpoint the worktree after every turn
	// ({"enabled": false} turns it off for a repository where that costs too much). On by default.
	Checkpoints *CheckpointsConfig `json:"checkpoints,omitempty"`
}

// CheckpointsConfig is process_config.checkpoints.
type CheckpointsConfig struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// CheckpointsOn says whether the project's jobs are checkpointed after every turn.
func (c ProjectConfig) CheckpointsOn() bool {
	return c.Checkpoints == nil || c.Checkpoints.Enabled == nil || *c.Checkpoints.Enabled
}

// CheckpointsOn is ProjectConfig.CheckpointsOn for a project; one whose configuration cannot be
// read is checkpointed, as every project is by default.
func (s *Service) CheckpointsOn(ctx context.Context, projectID pgtype.UUID) bool {
	c, err := s.projectConfig(ctx, projectID)
	return err != nil || c.CheckpointsOn()
}

// ParseProjectConfig reads process_config; empty input is an empty config.
func ParseProjectConfig(b []byte) (ProjectConfig, error) {
	var c ProjectConfig
	if len(b) == 0 {
		return c, nil
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("process_config: %w", err)
	}
	if c.Policy != nil {
		switch c.Policy.Placement {
		case "", "local", "cloud":
		default:
			return c, fmt.Errorf("process_config: policy.placement must be local or cloud, not %q", c.Policy.Placement)
		}
	}
	return c, nil
}

func (s *Service) projectConfig(ctx context.Context, projectID pgtype.UUID) (ProjectConfig, error) {
	p, err := db.New(s.pool).GetProject(ctx, projectID)
	if err != nil {
		return ProjectConfig{}, err
	}
	return ParseProjectConfig(p.ProcessConfig)
}

// RoleGuide is the project's prompt for a role, falling back to the built-in one.
func (s *Service) RoleGuide(ctx context.Context, projectID pgtype.UUID, role string) (string, error) {
	c, err := s.projectConfig(ctx, projectID)
	if err != nil {
		return "", err
	}
	if g := strings.TrimSpace(c.Roles[role]); g != "" {
		return g, nil
	}
	return DefaultRoleGuide(role), nil
}

// Autopilot reports whether the project queues jobs automatically and with which backend.
// It is on by default; a project turns it off with {"autopilot":{"enabled":false}}.
func (s *Service) Autopilot(ctx context.Context, projectID pgtype.UUID, defaultBackend string) (bool, string, error) {
	c, err := s.projectConfig(ctx, projectID)
	if err != nil {
		return false, "", err
	}
	enabled := true
	if c.Autopilot != nil && c.Autopilot.Enabled != nil {
		enabled = *c.Autopilot.Enabled
	}
	backend := c.PolicyFor("", defaultBackend).Backend
	return enabled && backend != "", backend, nil
}

// ContextDoc is a document from earlier work that a job should read.
type ContextDoc struct {
	Kind  string // artifact type ("brief", "plan", "report", "review") or "rollback"
	Phase string
	Title string
	Body  string
	// Origin says where it came from, in the words a person uses: task, catalogue, product,
	// knowledge, artifact. A person reading a prompt package wants to know which part of the
	// system they would have to change to change what the agent was told.
	Origin string
	// FullBytes is the document's length before any trimming.
	FullBytes int
	// Left, when not empty, says why a document was cut short or left out entirely. A package
	// that silently drops its tail looks the same as one that never had it.
	Left string
	// Dropped means it never reached the prompt at all.
	Dropped bool
	// Entry is the Brain entry it is, when it is one: what a job is recorded as having been handed.
	Entry pgtype.UUID
	// Private is a person's own context, lent to the agent of their turn: it reaches the prompt
	// and never the job's record, which the project reads.
	Private bool
}

// Reasons a document did not arrive whole. They are sentences because they are read by people.
const (
	leftTooLong = "too long on its own; the rest was cut"
	leftNoRoom  = "no room left in the package"
)

const (
	maxDocBytes     = 20000
	maxContextBytes = 60000
)

// ContextRequest is what a job's documents are gathered for.
type ContextRequest struct {
	Task pgtype.UUID
	Role string
	// Job is the job they are for, if it exists yet; Agent is its agent, whose own lessons come
	// with it; OnBehalfOf is the person its turn acts for, whose private context it is lent.
	Job        pgtype.UUID
	Agent      pgtype.UUID
	OnBehalfOf pgtype.UUID
}

// JobContext collects what a new job of role in the task's current phase would know, for a job
// nobody in particular asked for: see ContextFor.
func (s *Service) JobContext(ctx context.Context, taskID pgtype.UUID, role string) ([]ContextDoc, error) {
	return s.ContextFor(ctx, ContextRequest{Task: taskID, Role: role})
}

// ContextFor collects what a job should know: why its phase was sent back, the part of the
// product it is about, what the Brain holds for it, how this workspace builds software, the
// working state and the latest version of every artifact produced so far. Documents are
// truncated so the prompt stays bounded.
func (s *Service) ContextFor(ctx context.Context, req ContextRequest) ([]ContextDoc, error) {
	taskID, role := req.Task, req.Role
	q := db.New(s.pool)
	t, err := q.GetTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	arts, err := q.ListArtifacts(ctx, taskID)
	if err != nil {
		return nil, err
	}
	latest := map[string]db.Artifact{}
	var keys []string
	for _, a := range arts {
		k := a.Phase + "/" + a.Type
		if _, ok := latest[k]; !ok {
			keys = append(keys, k)
		}
		if cur, ok := latest[k]; !ok || a.Version > cur.Version {
			latest[k] = a
		}
	}
	var docs []ContextDoc
	total := 0
	add := func(d ContextDoc) {
		d.FullBytes = max(d.FullBytes, len(d.Body))
		if d.Dropped || total >= maxContextBytes {
			// Kept in the list, with nothing in it: a person looking at what the agent was
			// told should see what it was not told, and why.
			if !d.Dropped {
				d.Left = leftNoRoom
			}
			d.Body, d.Dropped = "", true
			docs = append(docs, d)
			return
		}
		if len(d.Body) > maxDocBytes {
			d.Body = d.Body[:maxDocBytes] + "\n… [truncated]"
			d.Left = leftTooLong
		}
		if total+len(d.Body) > maxContextBytes {
			d.Body = d.Body[:maxContextBytes-total] + "\n… [truncated]"
			d.Left = leftNoRoom
		}
		total += len(d.Body)
		docs = append(docs, d)
	}
	if reason, err := q.LastRollbackReason(ctx, db.LastRollbackReasonParams{TaskID: taskID, ToPhase: t.Phase}); err == nil && reason != nil && *reason != "" {
		add(ContextDoc{Kind: "rollback", Phase: t.Phase, Title: "Why this phase was sent back", Body: *reason, Origin: "task"})
	}
	// Which part of the product this task is about, before anything about how to build it: an
	// agent told the repository, the owner and what a change here reaches spends no turns
	// working that out, and reads the neighbours it would otherwise break.
	if doc, ok, err := s.Catalogue(ctx, t); err != nil {
		return nil, err
	} else if ok {
		doc.Origin = "catalogue"
		add(doc)
	}
	// What the Brain holds for this job: its component and the neighbours, what the product
	// decided (for the roles that weigh direction), how the project runs, the agent's lessons and
	// the people's context — the asker's private context only for the asker's turn.
	brainDocs, err := s.brainContext(ctx, t, req)
	if err != nil {
		return nil, err
	}
	for _, d := range brainDocs {
		add(d)
	}

	// How this workspace builds software; the same for every project that enables it.
	know, _, err := s.Knowledge(ctx, t.ProjectID, role, t.Phase)
	if err != nil {
		return nil, err
	}
	for _, d := range know {
		if d.Origin == "" {
			d.Origin = "knowledge"
		}
		add(d)
	}

	// The working state comes first: it is the shortest route to where the task stands.
	if ws, ok := latest["task/working_state"]; ok && ws.Content != nil {
		add(ContextDoc{Kind: "working_state", Phase: "task", Title: fmt.Sprintf("Working state (v%d)", ws.Version), Body: *ws.Content, Origin: "artifact"})
	}
	for _, k := range keys {
		if k == "task/working_state" {
			continue
		}
		a := latest[k]
		body := ""
		if a.Content != nil {
			body = *a.Content
		}
		if a.Url != nil && *a.Url != "" {
			body += "\n\nLink: " + *a.Url
		}
		status := "draft"
		if a.ApprovedAt.Valid {
			status = "approved"
		}
		add(ContextDoc{Kind: a.Type, Phase: a.Phase, Title: fmt.Sprintf("%s from %s (v%d, %s)", a.Type, a.Phase, a.Version, status), Body: body, Origin: "artifact"})
	}
	return docs, nil
}

// brainContext is the Brain's part of a job's documents, with a note on citing it first.
func (s *Service) brainContext(ctx context.Context, t db.Task, req ContextRequest) ([]ContextDoc, error) {
	if s.brain == nil {
		return nil, nil
	}
	components, err := s.componentKeys(ctx, t)
	if err != nil {
		return nil, err
	}
	items, err := s.brain.Pack(ctx, brain.Request{Reader: brain.AgentReader(t.ProjectID, req.OnBehalfOf, req.Job),
		Components: components, Agent: req.Agent, Product: readsProduct(req.Role)})
	if err != nil || len(items) == 0 {
		return nil, err
	}
	docs := []ContextDoc{{Kind: "brain_note", Phase: "brain", Title: "Citing the Brain", Body: brain.CiteNote, Origin: "brain"}}
	for _, it := range items {
		origin := "brain"
		if it.Kind == "product" {
			origin = "product"
		}
		docs = append(docs, ContextDoc{Kind: it.Kind, Phase: it.Phase, Title: it.Title, Body: it.Body, Origin: origin,
			FullBytes: it.FullBytes, Left: it.Left, Dropped: it.Dropped, Entry: it.Entry.ID, Private: it.Private})
	}
	return docs, nil
}

// componentKeys are the task's component, then what it depends on and what depends on it: whose
// Brain entries a job about it reads. Nothing for a task about no component, or one that is
// only proposed.
func (s *Service) componentKeys(ctx context.Context, t db.Task) ([]string, error) {
	if !t.ComponentID.Valid {
		return nil, nil
	}
	q := db.New(s.pool)
	c, err := q.GetComponent(ctx, t.ComponentID)
	if err != nil || c.Status != "approved" {
		return nil, nil
	}
	ids := []pgtype.UUID{c.ID}
	deps, err := q.ListComponentDeps(ctx, ids)
	if err != nil {
		return nil, err
	}
	dependents, err := q.ListComponentDependents(ctx, ids)
	if err != nil {
		return nil, err
	}
	keys := []string{c.Key}
	for _, d := range deps {
		keys = append(keys, d.Component.Key)
	}
	for _, d := range dependents {
		keys = append(keys, d.Component.Key)
	}
	return keys, nil
}

// ArtifactTypeFor maps a runner role to the document it produces.
func ArtifactTypeFor(role string) string {
	switch role {
	case "researcher":
		return "brief"
	case "planner":
		return "plan"
	case "reviewer", "security_reviewer":
		return "review"
	default:
		return "report"
	}
}

// readsProduct says which roles are given the product's vision, principles and decisions.
// A worker follows a plan; a researcher, a planner and a reviewer weigh direction.
func readsProduct(role string) bool {
	switch role {
	case "researcher", "planner", "reviewer", "curator":
		return true
	}
	return false
}
