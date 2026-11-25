package conversations

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// workingState is the artifact a runner keeps for itself between jobs; nobody reads it in a
// conversation.
const workingState = "working_state"

// artifactReader is what the artifact cards read. *db.Queries is one.
type artifactReader interface {
	taskReader
	ListArtifacts(ctx context.Context, taskID pgtype.UUID) ([]db.Artifact, error)
}

// artifactCards shows what a phase produced (a brief, a plan, a report) as one card per
// document that moves to its newest version, with a preview and a diff of versions.
type artifactCards struct {
	artifacts artifactReader
}

// Events implements CardSource.
func (artifactCards) Events() []string { return []string{"artifact.created"} }

// Cards implements CardSource.
func (s artifactCards) Cards(ctx context.Context, e db.EventsAfterRow) ([]PlacedCard, error) {
	var payload struct {
		Phase string `json:"phase"`
		Type  string `json:"type"`
	}
	if err := json.Unmarshal(e.Payload, &payload); err != nil || payload.Type == workingState {
		return nil, nil
	}
	task, ok, err := taskOf(ctx, s.artifacts, e.AggregateID)
	if !ok {
		return nil, err
	}
	all, err := s.artifacts.ListArtifacts(ctx, task.ID)
	if err != nil {
		return nil, err
	}
	latest, found := newest(all, payload.Phase, payload.Type)
	if !found {
		return nil, nil
	}
	return []PlacedCard{{Task: task, Card: ArtifactCard(latest)}}, nil
}

func newest(all []db.Artifact, phase, typ string) (db.Artifact, bool) {
	var out db.Artifact
	found := false
	for _, a := range all {
		if a.Phase == phase && a.Type == typ && (!found || a.Version > out.Version) {
			out, found = a, true
		}
	}
	return out, found
}

// ArtifactCard is the card of one document of a phase, at its newest version.
func ArtifactCard(a db.Artifact) Card {
	actions := []string{"preview"}
	if a.Version > 1 {
		actions = append(actions, "diff")
	}
	return Card{
		Key:  fmt.Sprintf("artifact:%s:%s", a.Phase, a.Type),
		Type: CardArtifact,
		Body: fmt.Sprintf("%s · %s · v%d", a.Type, a.Phase, a.Version),
		Data: map[string]any{
			"ref":     Ref("artifact", uuidString(a.ID), map[string]any{"taskId": uuidString(a.TaskID)}),
			"title":   a.Type,
			"state":   fmt.Sprintf("v%d", a.Version),
			"summary": a.Phase,
			"version": a.Version,
			"actions": actions,
		},
	}
}
