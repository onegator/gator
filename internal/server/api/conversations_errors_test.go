package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/onegator/gator/internal/server/conversations"
)

// The conversations package's errors become their statuses; the store's own "no rows" still
// answers 404 wherever it reaches the API.
func TestFailConversationStatuses(t *testing.T) {
	s := &Server{Log: slog.Default()}
	for _, tc := range []struct {
		err  error
		want int
	}{
		{conversations.ErrNotFound, http.StatusNotFound},
		{fmt.Errorf("thread: %w", conversations.ErrNotFound), http.StatusNotFound},
		{pgx.ErrNoRows, http.StatusNotFound},
		{conversations.ErrForbidden, http.StatusForbidden},
		{fmt.Errorf("%w: bad", conversations.ErrInvalid), http.StatusBadRequest},
		{fmt.Errorf("%w: taken", conversations.ErrConflict), http.StatusConflict},
	} {
		rec := httptest.NewRecorder()
		s.failConversation(rec, tc.err)
		if rec.Code != tc.want {
			t.Errorf("%v: %d, want %d", tc.err, rec.Code, tc.want)
		}
	}
}
