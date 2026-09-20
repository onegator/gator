package process

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// Catalogue builds the slice of the component catalogue a job should read: the component the
// task is about, what it depends on, what depends on it, and the decisions that shaped it.
//
// The slice, not the catalogue: a worker fixing a login bug gains nothing from the shape of
// the billing service, and every document it does not need is budget spent on noise. Tasks
// with no component get nothing, which is the same as today.
func (s *Service) Catalogue(ctx context.Context, t db.Task) (ContextDoc, bool, error) {
	if !t.ComponentID.Valid {
		return ContextDoc{}, false, nil
	}
	q := db.New(s.pool)
	c, err := q.GetComponent(ctx, t.ComponentID)
	if err != nil {
		return ContextDoc{}, false, nil // deleted between the task and the job; not an error
	}
	ids := []pgtype.UUID{c.ID}
	deps, err := q.ListComponentDeps(ctx, ids)
	if err != nil {
		return ContextDoc{}, false, err
	}
	dependents, err := q.ListComponentDependents(ctx, ids)
	if err != nil {
		return ContextDoc{}, false, err
	}
	decisions, err := q.ListComponentDecisions(ctx, ids)
	if err != nil {
		return ContextDoc{}, false, err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "**%s** (%s)", c.Name, c.Kind)
	if c.Repo != "" {
		fmt.Fprintf(&b, " · repository `%s`", c.Repo)
	}
	if c.Path != "" {
		fmt.Fprintf(&b, " · path `%s`", c.Path)
	}
	b.WriteString("\n")
	if owner := s.ownerName(ctx, c.OwnerID); owner != "" {
		fmt.Fprintf(&b, "\nOwned by %s.\n", owner)
	}
	if c.Notes != "" {
		fmt.Fprintf(&b, "\n%s\n", c.Notes)
	}
	if len(deps) > 0 {
		fmt.Fprintf(&b, "\nDepends on: %s\n", componentList(deps))
	}
	if len(dependents) > 0 {
		// The reason this is worth the tokens: what a change here can break.
		fmt.Fprintf(&b, "\nUsed by (a change here reaches them): %s\n", dependentList(dependents))
	}
	for _, d := range decisions {
		fmt.Fprintf(&b, "\n_Decision — %s:_ %s\n", d.ProductContext.Title, d.ProductContext.Content)
	}
	return ContextDoc{Kind: "catalogue", Phase: "component", Title: "The part of the product this task is about", Body: b.String()}, true, nil
}

func (s *Service) ownerName(ctx context.Context, id pgtype.UUID) string {
	if !id.Valid {
		return ""
	}
	u, err := db.New(s.pool).GetUser(ctx, id)
	if err != nil {
		return ""
	}
	return u.Name
}

func componentList(rows []db.ListComponentDepsRow) string {
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, describeComponent(r.Component))
	}
	return strings.Join(names, ", ")
}

func dependentList(rows []db.ListComponentDependentsRow) string {
	names := make([]string, 0, len(rows))
	for _, r := range rows {
		names = append(names, describeComponent(r.Component))
	}
	return strings.Join(names, ", ")
}

func describeComponent(c db.Component) string {
	if c.Repo == "" {
		return fmt.Sprintf("%s (%s)", c.Name, c.Kind)
	}
	return fmt.Sprintf("%s (%s, `%s`)", c.Name, c.Kind, c.Repo)
}
