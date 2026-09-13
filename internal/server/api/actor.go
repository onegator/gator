package api

import (
	"context"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/process"
)

// ActorFromContext maps the authenticated principal to the process actor recorded on
// transitions. Anonymous requests act as system (and are rejected by handlers that mutate).
func ActorFromContext(ctx context.Context) process.Actor {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return process.Actor{Kind: process.ActorSystem}
	}
	switch p.Kind {
	case auth.KindUser:
		return process.Actor{Kind: process.ActorUser, ID: p.UserID}
	case auth.KindRunner:
		return process.Actor{Kind: process.ActorRunner, ID: p.TokenID}
	case auth.KindPlugin:
		return process.Actor{Kind: process.ActorPlugin, ID: p.TokenID}
	}
	return process.Actor{Kind: process.ActorSystem}
}
