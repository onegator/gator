package process

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

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
	enabled, backend := true, defaultBackend
	if c.Autopilot != nil {
		if c.Autopilot.Enabled != nil {
			enabled = *c.Autopilot.Enabled
		}
		if c.Autopilot.Backend != "" {
			backend = c.Autopilot.Backend
		}
	}
	return enabled && backend != "", backend, nil
}

// ContextDoc is a document from earlier work that a job should read.
type ContextDoc struct {
	Kind  string // artifact type ("brief", "plan", "report", "review") or "rollback"
	Phase string
	Title string
	Body  string
}

const (
	maxDocBytes     = 20000
	maxContextBytes = 60000
)

// JobContext collects what a new job in the task's current phase should know: the latest
// version of every artifact produced so far, and why the task was last sent back to this
// phase. Documents are truncated so the prompt stays bounded.
func (s *Service) JobContext(ctx context.Context, taskID pgtype.UUID) ([]ContextDoc, error) {
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
		if total >= maxContextBytes {
			return
		}
		if len(d.Body) > maxDocBytes {
			d.Body = d.Body[:maxDocBytes] + "\n… [truncated]"
		}
		if total+len(d.Body) > maxContextBytes {
			d.Body = d.Body[:maxContextBytes-total] + "\n… [truncated]"
		}
		total += len(d.Body)
		docs = append(docs, d)
	}
	if reason, err := q.LastRollbackReason(ctx, db.LastRollbackReasonParams{TaskID: taskID, ToPhase: t.Phase}); err == nil && reason != nil && *reason != "" {
		add(ContextDoc{Kind: "rollback", Phase: t.Phase, Title: "Why this phase was sent back", Body: *reason})
	}
	for _, k := range keys {
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
		add(ContextDoc{Kind: a.Type, Phase: a.Phase, Title: fmt.Sprintf("%s from %s (v%d, %s)", a.Type, a.Phase, a.Version, status), Body: body})
	}
	return docs, nil
}

// ArtifactTypeFor maps a runner role to the document it produces.
func ArtifactTypeFor(role string) string {
	switch role {
	case "researcher":
		return "brief"
	case "planner":
		return "plan"
	case "reviewer":
		return "review"
	default:
		return "report"
	}
}
