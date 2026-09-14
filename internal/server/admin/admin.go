// Package admin is a minimal HTML panel for M1 and debugging. It will be removed once
// Gator.app covers configuration (M5). It reuses the API's process service directly.
package admin

import (
	"embed"
	"html/template"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/store/db"
)

//go:embed templates/*.html
var files embed.FS

var tmpl = template.Must(template.ParseFS(files, "templates/*.html"))

// Handler serves /admin.
type Handler struct {
	Pool    *pgxpool.Pool
	Process *process.Service
}

// Router mounts the panel.
func (h *Handler) Router() http.Handler {
	r := chi.NewRouter()
	r.Get("/", h.index)
	r.Get("/tasks/{id}", h.task)
	return r
}

func (h *Handler) index(w http.ResponseWriter, r *http.Request) {
	projects, err := db.New(h.Pool).ListProjects(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	inbox, err := h.Process.Inbox(r.Context(), pgtype.UUID{})
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	_ = tmpl.ExecuteTemplate(w, "index.html", map[string]any{"Projects": projects, "Inbox": inbox})
}

func (h *Handler) task(w http.ResponseWriter, r *http.Request) {
	var id pgtype.UUID
	if err := id.Scan(chi.URLParam(r, "id")); err != nil {
		http.NotFound(w, r)
		return
	}
	d, err := h.Process.Detail(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	trs, _ := h.Process.Transitions(r.Context(), id)
	m, _ := h.Process.Metrics(r.Context(), id, time.Now())
	_ = tmpl.ExecuteTemplate(w, "task.html", map[string]any{"D": d, "Transitions": trs, "M": m})
}
