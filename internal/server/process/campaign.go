package process

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// campaignCheck is the gate check on a campaign's Execution phase: it passes when every target
// is finished or explicitly skipped. The campaign is one decision, so it has one gate, and the
// check is what makes "all of it, or say why not" enforceable rather than a habit.
const campaignCheck = "campaign"

// CampaignKind is the task kind whose work is carried out in many places at once.
const CampaignKind = "campaign"

// executionPhase is the only phase the campaign check belongs to. Planning is where the targets
// are chosen, so a half-filled list must not block the campaign from starting.
const executionPhase = "execution"

// Target is one place a campaign has to reach.
type Target struct {
	// Key is how the person named it: a component key of the campaign's project, or the slug
	// of another project.
	Key string
	// ProjectID is where the child task is created. Zero means the campaign's own project.
	ProjectID pgtype.UUID
	// Title and Instruction describe the work there. Empty title takes the campaign's.
	Title       string
	Instruction string
	// Kind of the child task; empty means chore, which is what most fleet changes are.
	Kind string
}

// CampaignProgress is what the task detail shows: how far the campaign has got.
type CampaignProgress struct {
	Targets []TargetState
	Done    int
	Blocked int
	Waiting int
	Skipped int
}

// TargetState is one target and what has become of it.
type TargetState struct {
	Key        string
	ProjectID  pgtype.UUID
	TaskID     pgtype.UUID
	Title      string
	Phase      string
	Closed     bool
	Blocked    string
	Skipped    bool
	SkipReason string
}

var errNotACampaign = errors.New("not a campaign task")

// AddTargets gives a campaign the places it has to reach, creating one task for each. Adding a
// target that is already there does nothing, so this is safe to call again after a partial
// failure — half a fleet change is the worst outcome of all.
func (s *Service) AddTargets(ctx context.Context, campaignID pgtype.UUID, targets []Target, actor Actor) (CampaignProgress, error) {
	q := db.New(s.pool)
	campaign, err := q.GetTask(ctx, campaignID)
	if err != nil {
		return CampaignProgress{}, err
	}
	if campaign.Kind != CampaignKind {
		return CampaignProgress{}, errNotACampaign
	}
	if campaign.ClosedAt.Valid {
		return CampaignProgress{}, ErrTaskClosed
	}
	existing, err := q.ListCampaignTargets(ctx, campaignID)
	if err != nil {
		return CampaignProgress{}, err
	}
	known := map[string]bool{}
	for _, t := range existing {
		known[t.CampaignTarget.TargetKey] = true
	}
	for _, t := range targets {
		key := strings.TrimSpace(t.Key)
		if key == "" || known[key] {
			continue
		}
		projectID := t.ProjectID
		if !projectID.Valid {
			projectID = campaign.ProjectID
		}
		kind := t.Kind
		if kind == "" {
			kind = "chore"
		}
		title := t.Title
		if title == "" {
			title = campaign.Title + ": " + key
		}
		child, err := s.Create(ctx, CreateParams{
			ProjectID: projectID, Kind: kind, Title: title,
			Description:  childDescription(campaign, t),
			Urgency:      campaign.Urgency,
			SourceTaskID: pgtype.UUID{Bytes: campaignID.Bytes, Valid: true},
		}, actor)
		if err != nil {
			return CampaignProgress{}, fmt.Errorf("target %s: %w", key, err)
		}
		if _, err := q.AddCampaignTarget(ctx, db.AddCampaignTargetParams{
			CampaignID: campaignID, TargetKey: key, ProjectID: projectID,
			TaskID: pgtype.UUID{Bytes: child.ID.Bytes, Valid: true}}); err != nil {
			return CampaignProgress{}, err
		}
		known[key] = true
	}
	return s.refreshCampaign(ctx, campaignID)
}

// childDescription carries the campaign's instruction to each place, so an agent working one
// target has the whole decision in front of it rather than a title.
func childDescription(campaign db.Task, t Target) string {
	var b strings.Builder
	b.WriteString("Part of the campaign **" + campaign.Title + "**.\n\n")
	if t.Instruction != "" {
		b.WriteString(t.Instruction + "\n\n")
	} else if campaign.Description != "" {
		b.WriteString(campaign.Description + "\n\n")
	}
	b.WriteString("Target: " + t.Key)
	return b.String()
}

// SkipTarget drops one target from a campaign, with a reason. Approving a campaign needs every
// target finished or skipped, and a skip without a reason would let the hard half disappear.
func (s *Service) SkipTarget(ctx context.Context, campaignID pgtype.UUID, key, reason string, actor Actor) (CampaignProgress, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return CampaignProgress{}, errors.New("skipping a target requires a reason")
	}
	q := db.New(s.pool)
	row, err := q.SkipCampaignTarget(ctx, db.SkipCampaignTargetParams{
		CampaignID: campaignID, TargetKey: key, SkipReason: reason})
	if errors.Is(err, pgx.ErrNoRows) {
		return CampaignProgress{}, fmt.Errorf("target %q is not waiting on this campaign", key)
	}
	if err != nil {
		return CampaignProgress{}, err
	}
	// The work there stops too: leaving an open task for a target somebody decided against is
	// how an inbox fills with things nobody means to do.
	if row.TaskID.Valid {
		if _, err := s.Close(ctx, row.TaskID, actor, "skipped in the campaign: "+reason); err != nil {
			return CampaignProgress{}, err
		}
	}
	return s.refreshCampaign(ctx, campaignID)
}

// CampaignProgressFor reads a campaign's state without changing anything.
func (s *Service) CampaignProgressFor(ctx context.Context, campaignID pgtype.UUID) (CampaignProgress, error) {
	rows, err := db.New(s.pool).ListCampaignTargets(ctx, campaignID)
	if err != nil {
		return CampaignProgress{}, err
	}
	return progressOf(rows), nil
}

// OnCampaignChildChanged updates the campaign's gate after one of its children moves. A child
// finishing is the only thing that can make a campaign ready, and nobody should have to press
// anything for the campaign to notice.
func (s *Service) OnCampaignChildChanged(ctx context.Context, childID pgtype.UUID) error {
	campaignID, err := db.New(s.pool).CampaignForChild(ctx, pgtype.UUID{Bytes: childID.Bytes, Valid: true})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // an ordinary task, not part of a campaign
	}
	if err != nil {
		return err
	}
	_, err = s.refreshCampaign(ctx, campaignID)
	return err
}

// refreshCampaign recomputes the progress and writes it onto the campaign's gate.
func (s *Service) refreshCampaign(ctx context.Context, campaignID pgtype.UUID) (CampaignProgress, error) {
	q := db.New(s.pool)
	rows, err := q.ListCampaignTargets(ctx, campaignID)
	if err != nil {
		return CampaignProgress{}, err
	}
	p := progressOf(rows)
	campaign, err := q.GetTask(ctx, campaignID)
	if err != nil {
		return p, err
	}
	if campaign.Phase != executionPhase || campaign.ClosedAt.Valid {
		return p, nil
	}
	status, detail := "pending", p.summary()
	switch {
	case len(p.Targets) == 0:
		status = "pending"
		detail = "no targets yet"
	case p.Done+p.Skipped == len(p.Targets):
		status = "pass"
	default:
		status = "fail"
	}
	if err := s.SetCheck(ctx, campaignID, Check{Name: campaignCheck, Source: "campaign", Status: status, Detail: detail}); err != nil {
		// A campaign whose own task has been closed or removed is not an error worth failing
		// the caller's write for; the progress it asked about is still true.
		if !errors.Is(err, ErrTaskClosed) {
			return p, err
		}
	}
	return p, nil
}

func progressOf(rows []db.ListCampaignTargetsRow) CampaignProgress {
	var p CampaignProgress
	for _, r := range rows {
		t := r.CampaignTarget
		state := TargetState{Key: t.TargetKey, ProjectID: t.ProjectID, TaskID: t.TaskID,
			Skipped: t.SkippedAt.Valid, SkipReason: t.SkipReason}
		if r.Title != nil {
			state.Title = *r.Title
		}
		if r.Phase != nil {
			state.Phase = *r.Phase
		}
		state.Closed = r.ClosedAt.Valid
		if r.BlockedReason != nil {
			state.Blocked = *r.BlockedReason
		}
		switch {
		case state.Skipped:
			p.Skipped++
		case state.Closed:
			p.Done++
		case state.Blocked != "":
			p.Blocked++
		default:
			p.Waiting++
		}
		p.Targets = append(p.Targets, state)
	}
	return p
}

// summary is the line a person reads on the gate: where the fleet change has got to.
func (p CampaignProgress) summary() string {
	parts := []string{fmt.Sprintf("%d of %d done", p.Done, len(p.Targets))}
	if p.Blocked > 0 {
		parts = append(parts, fmt.Sprintf("%d blocked", p.Blocked))
	}
	if p.Waiting > 0 {
		parts = append(parts, fmt.Sprintf("%d in progress", p.Waiting))
	}
	if p.Skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", p.Skipped))
	}
	return strings.Join(parts, ", ")
}
