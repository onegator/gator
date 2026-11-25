package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/credentials"
)

const payersToken = "sk-ant-oat01-PayersTokenTheAgentPrinted0123456789abcd"

// payersJob is a job leased with a person's login, and that person's id.
func (h *harness) payersJob() (job, user pgtype.UUID) {
	h.t.Helper()
	ctx := context.Background()
	id, _ := h.login("member")
	user = mustUUID(h.t, id)
	if _, err := h.api.Credentials.Put(ctx, user, credentials.ClaudeSubscription, payersToken); err != nil {
		h.t.Fatal(err)
	}
	if err := h.pool.QueryRow(ctx, `WITH p AS (INSERT INTO projects (slug, name) VALUES ('mask-'||gen_random_uuid(), 'M') RETURNING id),
		t AS (INSERT INTO tasks (project_id, kind, title, phase) SELECT id, 'bug', 'x', 'triage' FROM p RETURNING id, project_id)
		INSERT INTO jobs (task_id, project_id, phase, role, backend, instruction, created_by_kind, payer_id, credential_id, credential_kind, status)
		SELECT t.id, t.project_id, 'triage', 'triager', 'claude', '', 'user', $1,
		       (SELECT id FROM user_credentials WHERE user_id = $1), 'claude_subscription', 'running' FROM t RETURNING id`, user).Scan(&job); err != nil {
		h.t.Fatal(err)
	}
	return job, user
}

// What a job's agent sends with its own token — a message, a Brain entry, a call through the MCP
// proxy — is masked of its login before any handler sees it; anybody else's request is not touched.
func TestAJobsWritesAreMaskedOfItsLogin(t *testing.T) {
	h := newHarness(t)
	job, user := h.payersJob()
	var seen string
	handler := h.api.maskJobLogins(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = string(b)
	}))
	send := func(p auth.Principal) {
		req := httptest.NewRequest("POST", "/api/v1/messages", strings.NewReader(`{"body":"my login is `+payersToken+`"}`))
		handler.ServeHTTP(httptest.NewRecorder(), req.WithContext(auth.WithPrincipal(req.Context(), p)))
	}
	send(auth.Principal{Kind: auth.KindJob, JobID: job})
	if strings.Contains(seen, payersToken) || !strings.Contains(seen, "[redacted]") {
		t.Fatalf("the job wrote %s", seen)
	}
	send(auth.Principal{Kind: auth.KindUser, UserID: user})
	if !strings.Contains(seen, payersToken) {
		t.Fatalf("a person's own request was changed: %s", seen)
	}
}
