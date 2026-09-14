package api

import (
	"encoding/json"

	"github.com/jackc/pgx/v5/pgtype"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/onegator/gator/internal/server/api/gen"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

func toUUID(u pgtype.UUID) openapi_types.UUID { return openapi_types.UUID(u.Bytes) }

func toUUIDPtr(u pgtype.UUID) *openapi_types.UUID {
	if !u.Valid {
		return nil
	}
	v := toUUID(u)
	return &v
}

func fromUUID(u openapi_types.UUID) pgtype.UUID { return pgtype.UUID{Bytes: u, Valid: true} }

func toProject(p db.Project) gen.Project {
	return gen.Project{Id: toUUID(p.ID), Slug: p.Slug, Name: p.Name, Tags: p.Tags, CreatedAt: p.CreatedAt.Time}
}

func toTask(t db.Task) gen.Task {
	out := gen.Task{
		Id: toUUID(t.ID), ProjectId: toUUID(t.ProjectID), Kind: t.Kind, Title: t.Title, Description: &t.Description, Phase: t.Phase,
		Urgency: int(t.Urgency), OwnerKind: t.OwnerKind, OwnerId: toUUIDPtr(t.OwnerID),
		RequirementsChanged: t.RequirementsChanged, BlockedReason: t.BlockedReason,
		PhaseEnteredAt: t.PhaseEnteredAt.Time, CreatedAt: t.CreatedAt.Time,
	}
	if t.ClosedAt.Valid {
		c := t.ClosedAt.Time
		out.ClosedAt = &c
	}
	return out
}

func toGate(g db.Gate, p process.Phase) gen.Gate {
	var checks []process.Check
	if len(g.Checks) > 0 {
		_ = json.Unmarshal(g.Checks, &checks)
	}
	out := gen.Gate{Phase: g.Phase, Kind: gen.GateKind(p.Gate), HumanApproved: g.HumanApprovedAt.Valid,
		BlockedReason: g.BlockedReason, BlockedBy: g.BlockedBy, Checks: []gen.Check{}}
	for _, c := range checks {
		cc := gen.Check{Name: c.Name, Source: c.Source, Status: gen.CheckStatus(c.Status)}
		if c.Detail != "" {
			d := c.Detail
			cc.Detail = &d
		}
		out.Checks = append(out.Checks, cc)
	}
	return out
}

func toDetail(d process.Detail) gen.TaskDetail {
	t := toTask(d.Task)
	phases := make([]string, 0, len(d.Phases))
	for _, p := range d.Phases {
		phases = append(phases, p.Name)
	}
	return gen.TaskDetail{
		Id: t.Id, ProjectId: t.ProjectId, Kind: t.Kind, Title: t.Title, Phase: t.Phase, Urgency: t.Urgency,
		OwnerKind: t.OwnerKind, OwnerId: t.OwnerId, RequirementsChanged: t.RequirementsChanged,
		BlockedReason: t.BlockedReason, PhaseEnteredAt: t.PhaseEnteredAt, CreatedAt: t.CreatedAt, ClosedAt: t.ClosedAt,
		Gate: toGate(d.Gate, d.Phase), Phases: phases,
	}
}

func toTransition(tr db.PhaseTransition) gen.Transition {
	return gen.Transition{Id: tr.ID, FromPhase: tr.FromPhase, ToPhase: tr.ToPhase, Kind: tr.Kind,
		ActorKind: tr.ActorKind, ActorId: toUUIDPtr(tr.ActorID), Reason: tr.Reason, CreatedAt: tr.CreatedAt.Time}
}

func toArtifact(a db.Artifact) gen.Artifact {
	out := gen.Artifact{Id: toUUID(a.ID), Phase: a.Phase, Type: a.Type, Version: int(a.Version), Content: a.Content, Url: a.Url,
		Approved: a.ApprovedAt.Valid, CreatedAt: a.CreatedAt.Time}
	if a.ApprovedAt.Valid {
		v := a.ApprovedAt.Time
		out.ApprovedAt = &v
	}
	return out
}
