package api

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/process"
)

type actorKey struct{}

// ActorFromContext returns the acting subject. Until PLQ-218 lands, identity comes from the
// X-Gator-User header (a user UUID); requests without it act as `system`.
// TODO(PLQ-218): replace with OIDC sessions and bearer tokens.
func ActorFromContext(ctx context.Context) process.Actor {
	if a, ok := ctx.Value(actorKey{}).(process.Actor); ok {
		return a
	}
	return process.Actor{Kind: process.ActorSystem}
}

func devActorMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor := process.Actor{Kind: process.ActorSystem}
		if h := r.Header.Get("X-Gator-User"); h != "" {
			var id pgtype.UUID
			if err := id.Scan(h); err == nil {
				actor = process.Actor{Kind: process.ActorUser, ID: id}
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), actorKey{}, actor)))
	})
}
