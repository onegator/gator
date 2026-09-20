package process

import (
	"context"
	"sort"

	"github.com/jackc/pgx/v5/pgtype"
)

// PhaseView is one phase of a project's process, as clients draw it. A phase that needs a
// plugin capability the project lacks is listed but inactive, so people can see what the
// process would look like with that plugin.
type PhaseView struct {
	Name     string
	Owner    string
	Role     string
	Gate     string
	Requires string
	Active   bool
}

// TemplateView is the process for one kind of task in a project.
type TemplateView struct {
	Kind         string
	MaxRollbacks int
	Phases       []PhaseView
	// Source says whether this process comes from the project, the workspace or the defaults.
	// Without it nobody can tell what a project override would be replacing.
	Source string
}

// sourcedResolver is a TemplateResolver that can also say where each template came from.
type sourcedResolver interface {
	CatalogWithSources(ctx context.Context, projectID pgtype.UUID) (Catalog, map[string]TemplateSource, error)
}

// Templates returns the project's process per kind of task, in phase order.
func (s *Service) Templates(ctx context.Context, projectID pgtype.UUID) ([]TemplateView, error) {
	var sources map[string]TemplateSource
	catalog, err := s.templates.Catalog(ctx, projectID)
	if sr, ok := s.templates.(sourcedResolver); ok {
		catalog, sources, err = sr.CatalogWithSources(ctx, projectID)
	}
	if err != nil {
		return nil, err
	}
	caps, err := s.caps.Capabilities(ctx, projectID)
	if err != nil {
		return nil, err
	}
	enabled := map[string]bool{}
	for _, c := range caps {
		enabled[c] = true
	}
	kinds := make([]string, 0, len(catalog))
	for kind := range catalog {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	out := make([]TemplateView, 0, len(kinds))
	for _, kind := range kinds {
		t := catalog[kind]
		source := sources[kind]
		if source == "" {
			source = SourceDefault
		}
		view := TemplateView{Kind: kind, MaxRollbacks: t.MaxRollbacks, Source: string(source)}
		for _, p := range t.Phases {
			view.Phases = append(view.Phases, PhaseView{
				Name: p.Name, Owner: string(p.Owner), Role: p.Role, Gate: string(p.Gate),
				Requires: p.Requires, Active: p.Requires == "" || enabled[p.Requires],
			})
		}
		out = append(out, view)
	}
	return out, nil
}
