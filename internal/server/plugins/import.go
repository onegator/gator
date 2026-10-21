package plugins

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/plugin"
)

// Import asks a project's plugin to bring in what already exists in the outside system: the
// issues opened before the plugin was switched on, which no webhook will ever mention. The
// plugin must make it idempotent, so it is safe to ask again.
func (h *Host) Import(ctx context.Context, projectID pgtype.UUID, name string) (plugin.ImportResult, error) {
	var out plugin.ImportResult
	r, err := db.New(h.pool).GetProjectPlugin(ctx, db.GetProjectPluginParams{ProjectID: projectID, Name: name})
	if err != nil {
		return out, err
	}
	i := h.get(r.ID)
	if i == nil {
		return out, fmt.Errorf("%w: switch %s on first", ErrNotRunning, name)
	}
	if !i.b.manifest.HasHook(plugin.MethodImport) {
		return out, fmt.Errorf("%w: %s has nothing to import", ErrInvalid, name)
	}
	err = h.callFor(ctx, i, plugin.MethodImport, plugin.ImportParams{}, &out, h.cfg.ImportTimeout)
	return out, err
}

// importLater imports in the background, as switching a plugin on or connecting it does: the
// answer to the person's request should not wait on a few hundred issues.
func (h *Host) importLater(projectID pgtype.UUID, name string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), h.cfg.ImportTimeout+h.cfg.CallTimeout)
		defer cancel()
		res, err := h.Import(ctx, projectID, name)
		var pe *plugin.Error
		switch {
		case err == nil:
		case errors.Is(err, ErrInvalid), errors.As(err, &pe) && pe.Code != plugin.CodeInternal:
			// Not configured far enough yet, such as no repository chosen: not a fault.
			h.log.Info("plugin import skipped", "plugin", name, "project", uuidString(projectID), "why", err)
			return
		default:
			h.log.Warn("plugin import", "plugin", name, "project", uuidString(projectID), "err", err)
			return
		}
		h.log.Info("plugin import", "plugin", name, "project", uuidString(projectID), "created", res.Created, "existing", res.Existing)
	}()
}
