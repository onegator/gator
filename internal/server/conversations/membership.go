package conversations

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// Project membership events that put a person into the project's default channels.
var membershipEvents = []string{"project.member_set", "project.invitation_accepted"}

// defaultChannelJoiner puts whoever joins a project into its #general. Leaving a project needs
// nothing here: Access stops them reading its conversations the moment they are out.
type defaultChannelJoiner struct {
	directory *Directory
}

// Handle implements Handler.
func (j defaultChannelJoiner) Handle(ctx context.Context, e db.EventsAfterRow) error {
	var payload struct {
		UserID string `json:"userId"`
	}
	if err := json.Unmarshal(e.Payload, &payload); err != nil || !e.AggregateID.Valid {
		return nil
	}
	var user pgtype.UUID
	if err := user.Scan(payload.UserID); err != nil {
		return nil
	}
	return j.directory.JoinDefaultChannels(ctx, e.AggregateID, user)
}
