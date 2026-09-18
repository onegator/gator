package process

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/knowledge"
	"github.com/onegator/gator/internal/server/store/db"
)

// Knowledge is the technical part of a job's prompt: how this workspace builds software.
// A project's own entries come first and win; then the workspace's entries; then the packs
// the project switched on, filtered by the role, the phase and the project's tags.
//
// Warnings name the packs a project's entry overrode, so the Knowledge screen can show what
// is being shadowed instead of leaving it a mystery.
func (s *Service) Knowledge(ctx context.Context, projectID pgtype.UUID, role, phase string) (docs []ContextDoc, warnings []string, err error) {
	q := db.New(s.pool)
	project, err := q.GetProject(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	own, err := q.ListProjectKnowledge(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}
	shared, err := q.ListWorkspaceKnowledge(ctx)
	if err != nil {
		return nil, nil, err
	}
	packs, err := q.ListPacksForProject(ctx, projectID)
	if err != nil {
		return nil, nil, err
	}

	taken := map[string]string{} // scope/title → where it came from
	add := func(e db.KnowledgeEntry, where string) {
		key := e.Scope + "/" + e.Title
		if from, clash := taken[key]; clash {
			warnings = append(warnings, fmt.Sprintf("%s %q from %s is hidden by the one from %s", e.Scope, e.Title, where, from))
			return
		}
		taken[key] = where
		docs = append(docs, ContextDoc{Kind: "knowledge", Phase: e.Scope, Title: fmt.Sprintf("%s: %s", e.Scope, e.Title), Body: e.Content})
	}
	for _, e := range own {
		add(e, "this project")
	}
	for _, e := range shared {
		add(e, "the workspace")
	}
	for _, row := range packs {
		var m knowledge.Manifest
		if err := json.Unmarshal(row.Manifest, &m); err != nil {
			warnings = append(warnings, fmt.Sprintf("pack %s has an unreadable manifest", row.Name))
			continue
		}
		if !m.Applies(role, phase, project.Tags) {
			// Silence here looks like a broken screen, so say what the pack is waiting for.
			warnings = append(warnings, fmt.Sprintf("pack %s is on for this project but %s", row.Name, why(m, role, phase, project.Tags)))
			continue
		}
		key := m.Scope + "/" + m.Name
		if from, clash := taken[key]; clash {
			warnings = append(warnings, fmt.Sprintf("pack %s is hidden by the %s entry from %s", m.Name, m.Scope, from))
			continue
		}
		taken[key] = "a pack"
		var pack knowledge.Pack
		pack.Manifest = m
		if err := json.Unmarshal(row.Files, &pack.Files); err != nil {
			warnings = append(warnings, fmt.Sprintf("pack %s has unreadable files", row.Name))
			continue
		}
		docs = append(docs, ContextDoc{Kind: "knowledge", Phase: m.Scope,
			Title: fmt.Sprintf("%s: %s (pack %s)", m.Scope, m.Name, m.Version), Body: pack.Text()})
	}
	return docs, warnings, nil
}

// why says which of a pack's conditions this job did not meet.
func why(m knowledge.Manifest, role, phase string, tags []string) string {
	var unmet []string
	if len(m.Roles) > 0 && !slices.Contains(m.Roles, role) {
		unmet = append(unmet, fmt.Sprintf("it is for %s, not %s", strings.Join(m.Roles, " and "), role))
	}
	if len(m.Phases) > 0 && !slices.Contains(m.Phases, phase) {
		unmet = append(unmet, fmt.Sprintf("it is for %s, not %s", strings.Join(m.Phases, " and "), phaseOrAny(phase)))
	}
	if len(m.AppliesTo) > 0 && !overlaps(m.AppliesTo, tags) {
		unmet = append(unmet, fmt.Sprintf("it applies to projects tagged %s, and this one is tagged %s",
			strings.Join(m.AppliesTo, " or "), tagsOrNone(tags)))
	}
	if len(unmet) == 0 {
		return "it does not apply here"
	}
	return strings.Join(unmet, "; ")
}

func phaseOrAny(phase string) string {
	if phase == "" {
		return "any phase"
	}
	return phase
}

func tagsOrNone(tags []string) string {
	if len(tags) == 0 {
		return "nothing"
	}
	return strings.Join(tags, ", ")
}

func overlaps(a, b []string) bool {
	for _, x := range a {
		if slices.Contains(b, x) {
			return true
		}
	}
	return false
}
