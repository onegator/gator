package plugins

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/plugin"
)

// never forwarded to plugins
var privateHeaders = map[string]bool{"Authorization": true, "Cookie": true, "Proxy-Authorization": true}

// Hooks serves POST /hooks/{project}/{plugin}: the only public endpoint. The plugin checks
// the sender's signature; the core bounds the size and rate and handles each delivery once.
func (h *Host) Hooks() http.Handler {
	r := chi.NewRouter()
	r.Post("/{project}/{plugin}", h.webhook)
	return r
}

func hookJSON(w http.ResponseWriter, status int, v map[string]string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (h *Host) webhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	slug, name := chi.URLParam(r, "project"), chi.URLParam(r, "plugin")
	q := db.New(h.pool)
	row, err := q.GetProjectPluginBySlug(ctx, db.GetProjectPluginBySlugParams{Slug: slug, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		hookJSON(w, http.StatusNotFound, map[string]string{"error": "no such hook"})
		return
	}
	if err != nil {
		hookJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	var m plugin.Manifest
	_ = json.Unmarshal(row.Manifest, &m)
	i := h.get(row.ID)
	if !row.Enabled || !row.PluginEnabled || !m.HasHook(plugin.MethodWebhook) || i == nil {
		hookJSON(w, http.StatusNotFound, map[string]string{"error": "no such hook"})
		return
	}
	if !h.lim.Allow(slug + "/" + name) {
		hookJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limited"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.cfg.WebhookMaxBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			hookJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "body too large"})
			return
		}
		hookJSON(w, http.StatusBadRequest, map[string]string{"error": "unreadable body"})
		return
	}
	delivery := ""
	if m.Webhook != nil && m.Webhook.DeliveryHeader != "" {
		delivery = r.Header.Get(m.Webhook.DeliveryHeader)
	}
	if delivery == "" {
		delivery = r.Header.Get("X-Gator-Delivery")
	}
	if delivery == "" {
		sum := sha256.Sum256(body)
		delivery = "sha256:" + hex.EncodeToString(sum[:])
	}
	n, err := q.ClaimWebhookDelivery(ctx, db.ClaimWebhookDeliveryParams{ProjectPluginID: row.ID, DeliveryID: delivery})
	if err != nil {
		hookJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	if n == 0 {
		hookJSON(w, http.StatusOK, map[string]string{"status": "duplicate", "delivery_id": delivery})
		return
	}
	headers := map[string]string{}
	for k, v := range r.Header {
		if !privateHeaders[k] {
			headers[k] = strings.Join(v, ", ")
		}
	}
	err = h.call(ctx, i, plugin.MethodWebhook, plugin.WebhookParams{DeliveryID: delivery, Headers: headers, Body: body}, nil)
	if err != nil {
		// Let the sender retry: forget the delivery.
		_ = q.ReleaseWebhookDelivery(ctx, db.ReleaseWebhookDeliveryParams{ProjectPluginID: row.ID, DeliveryID: delivery})
		status := http.StatusBadGateway
		var re *plugin.Error
		switch {
		case errors.Is(err, ErrTimeout):
			status = http.StatusGatewayTimeout
		case errors.Is(err, ErrNotRunning):
			status = http.StatusServiceUnavailable
		case errors.As(err, &re) && (re.Code == plugin.CodeForbidden || re.Code == plugin.CodeInvalidParams):
			status = http.StatusBadRequest
		}
		hookJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	hookJSON(w, http.StatusOK, map[string]string{"status": "ok", "delivery_id": delivery})
}
