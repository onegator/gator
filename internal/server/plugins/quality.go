package plugins

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/quality"
	"github.com/onegator/gator/plugin"
)

// QualityChecks asks every plugin of the project that offers the hook what it knows about each
// component. A plugin that fails or times out is left out with a log line rather than failing
// the sweep: one broken plugin must not make a whole project look bad, and a missing answer is
// already distinct from a failing one.
func (h *Host) QualityChecks(ctx context.Context, projectID pgtype.UUID, components []string) []quality.Check {
	insts := h.forProject(projectID, plugin.MethodQualityEvaluate)
	if len(insts) == 0 {
		return nil
	}
	var (
		mu  sync.Mutex
		out []quality.Check
	)
	h.each(insts, func(i *instance) {
		var res plugin.QualityEvaluateResult
		if err := h.call(ctx, i, plugin.MethodQualityEvaluate, plugin.QualityEvaluateParams{Components: components}, &res); err != nil {
			if ctx.Err() == nil {
				i.log().Warn("asking a plugin for quality checks", "err", err)
			}
			return
		}
		mu.Lock()
		defer mu.Unlock()
		for _, c := range res.Checks {
			out = append(out, quality.Check{
				Component: c.Component, Rule: c.Rule, Source: i.b.manifest.Name,
				Status: c.Status, Detail: c.Detail, Weight: c.Weight})
		}
	})
	return out
}
