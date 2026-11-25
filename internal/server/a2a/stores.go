package a2a

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// remoteStore is what the outbound side reads and writes. *db.Queries is one.
type remoteStore interface {
	CreateRemoteAgent(ctx context.Context, arg db.CreateRemoteAgentParams) (db.Agent, error)
	GetAgent(ctx context.Context, id pgtype.UUID) (db.Agent, error)
	GetTurn(ctx context.Context, id pgtype.UUID) (db.AgentTurn, error)
	SetTurnStatus(ctx context.Context, arg db.SetTurnStatusParams) (db.AgentTurn, error)
	GetMessage(ctx context.Context, id pgtype.UUID) (db.Message, error)
	GetConversation(ctx context.Context, id pgtype.UUID) (db.Conversation, error)
	InsertA2ACall(ctx context.Context, arg db.InsertA2ACallParams) error
	ClaimA2ACall(ctx context.Context, arg db.ClaimA2ACallParams) (db.A2aCall, error)
	CountA2ACallsSince(ctx context.Context, arg db.CountA2ACallsSinceParams) (int64, error)
	SetA2ARemoteTask(ctx context.Context, arg db.SetA2ARemoteTaskParams) error
	EndA2ACall(ctx context.Context, arg db.EndA2ACallParams) error
	EndOpenA2ACall(ctx context.Context, arg db.EndOpenA2ACallParams) (int64, error)
}

// inboundStore is what the inbound side reads and writes outside a transaction. *db.Queries is one.
type inboundStore interface {
	GetA2AClient(ctx context.Context, id pgtype.UUID) (db.A2aClient, error)
	GetA2AClientByToken(ctx context.Context, tokenID pgtype.UUID) (db.A2aClient, error)
	ListA2AClients(ctx context.Context, projectID pgtype.UUID) ([]db.A2aClient, error)
	RevokeA2AClient(ctx context.Context, id pgtype.UUID) (db.A2aClient, error)
	ExternalCostSince(ctx context.Context, arg db.ExternalCostSinceParams) (float64, error)
	CountOpenA2ATasks(ctx context.Context, clientID pgtype.UUID) (int64, error)
	CreateA2ATask(ctx context.Context, arg db.CreateA2ATaskParams) (db.A2aTask, error)
	GetA2ATask(ctx context.Context, id pgtype.UUID) (db.A2aTask, error)
	GetExposedAgent(ctx context.Context, arg db.GetExposedAgentParams) (db.Agent, error)
	SetAgentExposed(ctx context.Context, arg db.SetAgentExposedParams) (db.Agent, error)
	GetMessage(ctx context.Context, id pgtype.UUID) (db.Message, error)
	GetTurnByTrigger(ctx context.Context, arg db.GetTurnByTriggerParams) (db.AgentTurn, error)
	JobCost(ctx context.Context, jobID pgtype.UUID) (db.JobCostRow, error)
}
