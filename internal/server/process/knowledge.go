package process

import (
	"context"
	"encoding/json"
	"fmt"

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
