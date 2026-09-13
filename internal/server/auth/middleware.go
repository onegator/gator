package auth

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/store/db"
)

// Authenticate resolves a principal from Authorization: Bearer or the session cookie.
// Anonymous requests continue without a principal; handlers decide what needs one.
// With devHeader on (GATOR_DEV_AUTH=1) X-Gator-User: <user uuid> acts as that user;
// never enable outside local development.
func Authenticate(tokens Tokens, devHeader bool, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var plaintext string
			if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
				plaintext = strings.TrimPrefix(h, "Bearer ")
			} else if c, err := r.Cookie(SessionCookie); err == nil {
				plaintext = c.Value
			}
			if plaintext != "" {
				p, err := tokens.Verify(r.Context(), plaintext)
				if err != nil {
					writeErr(w, http.StatusUnauthorized, "invalid or expired token")
					return
				}
				next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
				return
			}
			if devHeader {
				if h := r.Header.Get("X-Gator-User"); h != "" {
					var id pgtype.UUID
					if err := id.Scan(h); err == nil {
						u, err := db.New(tokens.Pool).GetUser(r.Context(), id)
						if err == nil {
							p := Principal{Kind: KindUser, UserID: u.ID, WorkspaceRole: u.WorkspaceRole, Name: u.Name, Scope: "user"}
							next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
							return
						}
					}
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAuth rejects anonymous requests.
func RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := FromContext(r.Context()); !ok {
			writeErr(w, http.StatusUnauthorized, "authentication required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireWorkspaceAdmin rejects everyone but workspace admins.
func RequireWorkspaceAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := FromContext(r.Context()); !ok || !p.IsWorkspaceAdmin() {
			writeErr(w, http.StatusForbidden, "workspace admin required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// AuditWith records mutations into audit_log.
func AuditWith(q *db.Queries, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
				next.ServeHTTP(w, r)
				return
			}
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			p, _ := FromContext(r.Context())
			actorKind := "anonymous"
			if p.Kind != "" {
				actorKind = string(p.Kind)
			}
			actorID := p.UserID
			if !actorID.Valid {
				actorID = p.TokenID
			}
			pattern := r.URL.Path
			if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
				pattern = rc.RoutePattern()
			}
			payload, _ := json.Marshal(map[string]any{"status": ww.Status(), "request_id": middleware.GetReqID(r.Context()), "scope": p.Scope})
			if err := q.InsertAudit(r.Context(), db.InsertAuditParams{
				ActorKind: actorKind, ActorID: actorID, Action: r.Method + " " + pattern, Target: r.URL.Path, Payload: payload,
			}); err != nil {
				log.Error("audit write failed", "err", err)
			}
		})
	}
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "code": http.StatusText(status)})
}
