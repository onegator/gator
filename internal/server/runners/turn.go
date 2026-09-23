package runners

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/store/db"
)

// A job ends because the agent decided it was finished. Nothing else was ever asked. The gate
// finds out afterwards, refuses the phase, and a person reads a report about work that was not
// done — ten minutes and one session too late.
//
// turn_ending is the question asked while the agent is still sitting in its worktree: does
// anything object? A plugin that watches CI knows the tests never ran; the core knows a
// planning phase produced a plan that this turn did not touch. An objection sends the agent
// back to work in the same session, where it is cheap, and it reaches the agent as data — the
// text comes from a plugin, which is to say from outside this workspace.
//
// The limit is what keeps this honest. Two objections per job by default: a plugin that
// disagrees for ever would hold an agent in a loop nobody asked for and spend a person's
// money doing it, so the third time the turn ends regardless and the receipt says so.

// turnEndingTimeout bounds the whole question. The runner is blocked on the answer with an
// agent session held open, so this is deliberately shorter than a plugin call's own timeout.
const turnEndingTimeout = 20 * time.Second

// turnEnding answers the runner's "may this turn end?". It always answers exactly once: a
// runner left waiting would hold a session open until its own timeout, which is the one
// failure mode worse than an objection nobody made.
func (s *session) turnEnding(te proto.TurnEnding) {
	answer := proto.Continue{JobID: te.JobID}
	defer func() {
		if err := s.send(proto.TypeContinue, answer); err != nil {
			s.m.log.Warn("answering a turn_ending", "job", te.JobID, "err", err)
		}
	}()

	jobID, ok := parseUUID(te.JobID)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, turnEndingTimeout)
	defer cancel()
	q := db.New(s.m.pool)
	job, err := q.GetJob(ctx, jobID)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			s.m.log.Warn("reading a job for turn_ending", "job", te.JobID, "err", err)
		}
		return
	}
	if job.RunnerID != s.runnerID || terminal(job.Status) {
		return
	}
	// Only a turn that thinks it succeeded is worth arguing with. A failed or stopped job has
	// a reason of its own, and sending it back to work would bury it.
	if te.Status != proto.StatusDone {
		return
	}

	limit := s.m.cfg.MaxObjections
	objections := s.objections(ctx, job, te)
	if len(objections) == 0 {
		return
	}
	if int(job.Objections) >= limit {
		answer.Exhausted = true
		answer.Note = fmt.Sprintf("%s — not raised: this job has used its %d objections", objections[0].Reason, limit)
		return
	}
	if _, err := q.BumpJobObjections(ctx, jobID); err != nil {
		// Without the count the limit cannot hold, and an objection loop is worse than a turn
		// that ends early. Let it end.
		s.m.log.Warn("counting an objection", "job", te.JobID, "err", err)
		return
	}
	answer.Objections = objections
}

// objections collects what the core says and what the project's plugins say.
func (s *session) objections(ctx context.Context, job db.Job, te proto.TurnEnding) []proto.Objection {
	var out []proto.Objection
	if o := s.coreObjection(ctx, job, te); o != nil {
		out = append(out, *o)
	}
	if j := s.m.cfg.Judge; j != nil {
		out = append(out, j.Objections(ctx, job, te)...)
	}
	return out
}

// coreObjection is the one objection Gator itself makes, and it is deliberately narrow: a
// phase that was planned produced a plan, and this turn committed nothing at all. That is not
// a judgement about whether the work is good — it is the one case where the work provably did
// not happen, and where the agent finishing anyway costs a person a whole phase.
//
// It says nothing about *which* steps of the plan were done. Matching a plan's prose against
// changed paths would be a guess, and a guess that sends an agent back to work is worse than
// silence.
func (s *session) coreObjection(ctx context.Context, job db.Job, te proto.TurnEnding) *proto.Objection {
	if len(te.Commits) > 0 || job.Phase != "implementation" {
		return nil
	}
	plan, err := db.New(s.m.pool).GetLatestArtifact(ctx, db.GetLatestArtifactParams{
		TaskID: job.TaskID, Phase: "planning", Type: "plan"})
	if err != nil || plan.Content == nil || strings.TrimSpace(*plan.Content) == "" {
		return nil
	}
	return &proto.Objection{Source: "gator", Reason: "This task was planned and this turn committed nothing. " +
		"Either carry out the plan from the planning phase and commit the work, or, if the plan cannot be " +
		"carried out, stop and say why with `gator-cli task blocked --reason \"...\"` instead of finishing."}
}
