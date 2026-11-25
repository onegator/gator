package plugins

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/plugin"
)

// Linkers answers whether a project's bridge redeems link codes: the plugin is switched on there and
// its manifest says links. It works from the database, on every node.
type Linkers struct{ Pool *pgxpool.Pool }

// RedeemsLinks implements bridges' linkers.
func (l Linkers) RedeemsLinks(ctx context.Context, project pgtype.UUID, provider string) (bool, error) {
	row, err := db.New(l.Pool).GetProjectPlugin(ctx, db.GetProjectPluginParams{ProjectID: project, Name: provider})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var m plugin.Manifest
	_ = json.Unmarshal(row.Manifest, &m)
	return row.Enabled && row.PluginEnabled && m.Links, nil
}
