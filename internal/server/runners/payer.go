package runners

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

// PayerQuestion is what decides who pays for a job: the task it works on and how it was asked for.
type PayerQuestion struct {
	Task      db.Task
	Kind      string
	CreatedBy process.Actor
	Turn      pgtype.UUID
	// PaidBy is a payer the caller already knows: the person whose turn a summary was written for.
	PaidBy pgtype.UUID
}

// PayerResolver says who pays for a job: whose model login it runs on. Zero is nobody.
type PayerResolver interface {
	Payer(ctx context.Context, q *db.Queries, j PayerQuestion) (pgtype.UUID, error)
}

// PayerRule is one rule of who pays: a person, or zero for "not for me to say".
type PayerRule interface {
	Payer(ctx context.Context, q *db.Queries, j PayerQuestion) (pgtype.UUID, error)
}

// PayerRuleFunc is a rule as a function.
type PayerRuleFunc func(ctx context.Context, q *db.Queries, j PayerQuestion) (pgtype.UUID, error)

// Payer implements PayerRule.
func (f PayerRuleFunc) Payer(ctx context.Context, q *db.Queries, j PayerQuestion) (pgtype.UUID, error) {
	return f(ctx, q, j)
}

// Payers asks its rules in order; the first to name a person decides.
type Payers []PayerRule

// Payer implements PayerResolver.
func (p Payers) Payer(ctx context.Context, q *db.Queries, j PayerQuestion) (pgtype.UUID, error) {
	for _, r := range p {
		who, err := r.Payer(ctx, q, j)
		if err != nil || who.Valid {
			return who, err
		}
	}
	return pgtype.UUID{}, nil
}

// DefaultPayers are Gator's rules, in order:
//
//  1. the payer the caller named (the person whose turn a summary serves);
//  2. the person who asked for the job: ran a task, asked an agent, answered a card;
//  3. for a turn an agent asked for, the person the chain of turns acts for;
//  4. for a task an automation created, the person who switched the automation on;
//  5. the person who created the task: its process phases, its summaries, its curation;
//  6. the person who allowed agents on the task (a task raised from outside has no creator).
func DefaultPayers() Payers {
	return Payers{PayerRuleFunc(namedPayer), PayerRuleFunc(requester), PayerRuleFunc(turnPrincipal),
		PayerRuleFunc(automationEnabler), PayerRuleFunc(taskCreator), PayerRuleFunc(agentsAllower)}
}

func namedPayer(_ context.Context, _ *db.Queries, j PayerQuestion) (pgtype.UUID, error) {
	return j.PaidBy, nil
}

func requester(_ context.Context, _ *db.Queries, j PayerQuestion) (pgtype.UUID, error) {
	if j.CreatedBy.Kind == process.ActorUser {
		return j.CreatedBy.ID, nil
	}
	return pgtype.UUID{}, nil
}

func turnPrincipal(ctx context.Context, q *db.Queries, j PayerQuestion) (pgtype.UUID, error) {
	if !j.Turn.Valid {
		return pgtype.UUID{}, nil
	}
	turn, err := q.GetTurn(ctx, j.Turn)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, nil
	}
	if err != nil {
		return pgtype.UUID{}, err
	}
	if turn.OnBehalfOf.Valid {
		return turn.OnBehalfOf, nil
	}
	if turn.RequestedByKind == string(process.ActorUser) {
		return turn.RequestedBy, nil
	}
	return pgtype.UUID{}, nil
}

func automationEnabler(ctx context.Context, q *db.Queries, j PayerQuestion) (pgtype.UUID, error) {
	if j.Task.Automation == nil {
		return pgtype.UUID{}, nil
	}
	a, err := q.GetProjectAutomation(ctx, db.GetProjectAutomationParams{ProjectID: j.Task.ProjectID, Key: *j.Task.Automation})
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, nil
	}
	return a.EnabledBy, err
}

func taskCreator(ctx context.Context, q *db.Queries, j PayerQuestion) (pgtype.UUID, error) {
	who, err := q.TaskCreator(ctx, j.Task.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgtype.UUID{}, nil
	}
	return who, err
}

func agentsAllower(_ context.Context, _ *db.Queries, j PayerQuestion) (pgtype.UUID, error) {
	return j.Task.AgentsAllowedBy, nil
}
