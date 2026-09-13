// Package httpapi exposes the HTTP surface of gator-server.
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/onegator/gator/internal/version"
)

// ReadyChecker reports whether a dependency is usable.
type ReadyChecker interface {
	Ready(ctx context.Context) error
}

// NewRouter builds the root router with health, readiness and version endpoints.
func NewRouter(db ReadyChecker) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", func(w http.ResponseWriter, req *http.Request) {
		ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
		defer cancel()
		checks := map[string]string{}
		status := http.StatusOK
		if err := db.Ready(ctx); err != nil {
			checks["database"] = err.Error()
			status = http.StatusServiceUnavailable
		} else {
			checks["database"] = "ok"
		}
		writeJSON(w, status, map[string]any{"checks": checks})
	})
	r.Get("/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"version": version.Version, "commit": version.Commit, "date": version.Date,
		})
	})
	return r
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
