package credentials

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/process"
	"github.com/onegator/gator/internal/server/runners"
	"github.com/onegator/gator/internal/server/secrets"
	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/internal/server/store/storetest"
)

// The package's tests switch the workspace's rule on and off: a database of their own.
func TestMain(m *testing.M) { storetest.Main(m, "credentials") }

type fixture struct {
	t       *testing.T
	ctx     context.Context
	pool    *pgxpool.Pool
	q       *db.Queries
	svc     *Service
	mgr     *runners.Manager
	proc    *process.Service
	project pgtype.UUID
	status  int // what the fake provider answers a check
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	url := os.Getenv("GATOR_DATABASE_URL")
	if url == "" {
		t.Skip("GATOR_DATABASE_URL not set")
	}
	ctx := context.Background()
	if err := store.Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	keys, err := secrets.ParseKeyring(secrets.NewKey("t"), "")
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, ctx: ctx, pool: pool, q: db.New(pool), status: http.StatusOK}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(f.status) }))
	t.Cleanup(provider.Close)
	f.svc = New(pool, Deps{Keys: keys, Kinds: DefaultKinds(Endpoints{Anthropic: provider.URL, OpenAI: provider.URL, HTTP: provider.Client()})})
	cat, err := process.DefaultCatalog()
	if err != nil {
		t.Fatal(err)
	}
	f.proc = process.NewService(pool, process.StaticCatalog(cat), process.StaticCapabilities(nil))
	f.mgr = runners.New(pool, f.proc, events.NewHub(), slog.New(slog.DiscardHandler), runners.Config{Credentials: f.svc})
	p, err := f.q.CreateProject(ctx, db.CreateProjectParams{Slug: "creds-" + suffix(), Name: "creds", Tags: []string{}, ProcessConfig: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	f.project = p.ID
	f.runnerLogins(false)
	return f
}

func suffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (f *fixture) user(name string) pgtype.UUID {
	f.t.Helper()
	var id pgtype.UUID
	if err := f.pool.QueryRow(f.ctx, "INSERT INTO users(email,name,workspace_role) VALUES($1,$2,'member') RETURNING id",
		name+"-"+suffix()+"@t.local", name).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) runnerLogins(on bool) {
	f.t.Helper()
	if err := f.svc.SetRunnerLogins(f.ctx, on, pgtype.UUID{}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) put(user pgtype.UUID, kind, raw string) View {
	f.t.Helper()
	v, err := f.svc.Put(f.ctx, user, kind, raw)
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

// job queues a job on a task created by creator, asked for by asker (zero: the system).
func (f *fixture) job(creator, asker pgtype.UUID, backend string) db.Job {
	f.t.Helper()
	actor := process.Actor{Kind: process.ActorSystem}
	if creator.Valid {
		actor = process.Actor{Kind: process.ActorUser, ID: creator}
	}
	task, err := f.proc.Create(f.ctx, process.CreateParams{ProjectID: f.project, Kind: "bug", Title: "Crash " + suffix()}, actor)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.q.AllowAgents(f.ctx, db.AllowAgentsParams{ID: task.ID}); err != nil {
		f.t.Fatal(err)
	}
	by := process.Actor{Kind: process.ActorSystem}
	if asker.Valid {
		by = process.Actor{Kind: process.ActorUser, ID: asker}
	}
	job, err := f.mgr.CreateJob(f.ctx, task.ID, runners.NewJob{Role: "implementer", Backend: backend, CreatedBy: by})
	if err != nil {
		f.t.Fatal(err)
	}
	return job
}

func (f *fixture) choose(job db.Job) runners.CredentialChoice {
	f.t.Helper()
	var c runners.CredentialChoice
	err := store.InTx(f.ctx, f.pool, func(q *db.Queries) error {
		var err error
		c, err = f.svc.Choose(f.ctx, q, job, true)
		return err
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// A login is kept encrypted, shown only by its kind and hint, and a person's own to remove.
func TestLoginsAreKeptSealedAndShownMasked(t *testing.T) {
	f := newFixture(t)
	ann := f.user("Ann")
	v := f.put(ann, ClaudeSubscription, claudeToken)
	if v.Hint != "…wxyz" || v.Status != StatusUnchecked || v.Label != "Claude subscription" || v.Backend != BackendClaude {
		t.Fatalf("view %+v", v)
	}
	var stored string
	if err := f.pool.QueryRow(f.ctx, "SELECT secret FROM user_credentials WHERE user_id = $1", ann).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, claudeToken) || !strings.HasPrefix(stored, "v1.") {
		t.Fatalf("stored in the clear: %s", stored)
	}
	if _, err := f.svc.Put(f.ctx, ann, ClaudeSubscription, anthropicKy); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an API key saved as a subscription: %v", err)
	}
	if _, err := f.svc.Put(f.ctx, ann, "gemini_key", "x"); !errors.Is(err, ErrUnknownKind) {
		t.Fatalf("unknown kind: %v", err)
	}
	list, _ := f.svc.List(f.ctx, ann)
	if len(list) != 1 {
		t.Fatalf("list %+v", list)
	}
	if err := f.svc.Delete(f.ctx, f.user("Bob"), ClaudeSubscription); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Bob removed Ann's login: %v", err)
	}
	if err := f.svc.Delete(f.ctx, ann, ClaudeSubscription); err != nil {
		t.Fatal(err)
	}
	if list, _ := f.svc.List(f.ctx, ann); len(list) != 0 {
		t.Fatalf("still there: %+v", list)
	}
}

// A check against the provider marks a refused login, and leaves it alone when it cannot tell.
func TestCheckMarksRefusedLogins(t *testing.T) {
	f := newFixture(t)
	ann := f.user("Ann")
	f.put(ann, AnthropicAPIKey, anthropicKy)
	if v, err := f.svc.Check(f.ctx, ann, AnthropicAPIKey); err != nil || v.Status != StatusOK || v.CheckedAt == nil {
		t.Fatalf("ok: %+v %v", v, err)
	}
	f.status = http.StatusBadGateway
	if v, err := f.svc.Check(f.ctx, ann, AnthropicAPIKey); !errors.Is(err, ErrUnavailable) || v.Status != StatusOK {
		t.Fatalf("502: %+v %v", v, err)
	}
	f.status = http.StatusUnauthorized
	v, err := f.svc.Check(f.ctx, ann, AnthropicAPIKey)
	if err != nil || v.Status != StatusError || !strings.Contains(v.LastError, "401") || strings.Contains(v.LastError, anthropicKy) {
		t.Fatalf("401: %+v %v", v, err)
	}
	f.put(ann, ChatGPTSubscription, chatgptAuth)
	if _, err := f.svc.Check(f.ctx, ann, ChatGPTSubscription); !errors.Is(err, ErrCannotCheck) {
		t.Fatalf("chatgpt: %v", err)
	}
}

// Who pays: the person who asked; for the autopilot's work, the task's creator. A job runs on its
// payer's preferred login for its backend, and waits, saying why, when they have none.
func TestJobsRunOnTheirPayersLoginOrWait(t *testing.T) {
	f := newFixture(t)
	ann, bob := f.user("Ann"), f.user("Bob")

	asked := f.job(ann, bob, BackendClaude)
	if asked.PayerID != bob {
		t.Fatalf("the person who asked does not pay: %v", asked.PayerID)
	}
	if !strings.Contains(asked.WaitingReason, "Waiting for Bob to connect a Claude subscription or an Anthropic API key") {
		t.Fatalf("waiting reason %q", asked.WaitingReason)
	}
	auto := f.job(ann, pgtype.UUID{}, BackendClaude)
	if auto.PayerID != ann {
		t.Fatalf("the task's creator does not pay for its phases: %v", auto.PayerID)
	}

	f.put(ann, AnthropicAPIKey, anthropicKy)
	if c := f.choose(auto); c.Kind != AnthropicAPIKey || c.Credential == nil || c.Credential.Env["ANTHROPIC_API_KEY"] != anthropicKy {
		t.Fatalf("Ann's job: %+v", c)
	}
	f.put(ann, ClaudeSubscription, claudeToken)
	if c := f.choose(auto); c.Kind != ClaudeSubscription || c.Credential.Env["CLAUDE_CODE_OAUTH_TOKEN"] != claudeToken {
		t.Fatalf("a subscription is preferred: %+v", c)
	}
	if c := f.choose(asked); c.Wait == "" {
		t.Fatalf("Bob's job ran on somebody's login: %+v", c)
	}
	// A refused login is skipped; with none left, the job waits rather than borrow the runner's.
	f.runnerLogins(true)
	if _, err := f.pool.Exec(f.ctx, "UPDATE user_credentials SET status = 'error', last_error = 'expired' WHERE user_id = $1", ann); err != nil {
		t.Fatal(err)
	}
	if c := f.choose(auto); !strings.Contains(c.Wait, "stopped working") || !strings.Contains(c.Wait, "expired") {
		t.Fatalf("refused logins: %+v", c)
	}
	// Bob has none of his own: with the workspace lending the runners' logins, his job runs on one.
	if c := f.choose(asked); !c.Runner() {
		t.Fatalf("lending: %+v", c)
	}
	// No kind runs gemini: only the runner's login can, and only when lent.
	gem := f.job(ann, ann, "acp:gemini")
	if c := f.choose(gem); !c.Runner() {
		t.Fatalf("gemini lent: %+v", c)
	}
	f.runnerLogins(false)
	if c := f.choose(gem); !strings.Contains(c.Wait, "acp:gemini") {
		t.Fatalf("gemini: %+v", c)
	}
}

// Saving a login releases its owner's waiting jobs from their reason; the next lease decides.
func TestConnectingALoginReleasesWaitingJobs(t *testing.T) {
	f := newFixture(t)
	ann := f.user("Ann")
	j := f.job(ann, ann, BackendCodex)
	if !strings.Contains(j.WaitingReason, "a ChatGPT subscription or an OpenAI API key") {
		t.Fatalf("reason %q", j.WaitingReason)
	}
	f.put(ann, OpenAIAPIKey, openaiKey)
	got, _ := f.q.GetJob(f.ctx, j.ID)
	if got.WaitingReason != "" {
		t.Fatalf("still waiting: %q", got.WaitingReason)
	}
}

// A ChatGPT login is held by one job at a time: the second waits, saying so, until the first ends.
// What the first rewrote replaces the stored login.
func TestAChatGPTLoginIsOneJobsAtATime(t *testing.T) {
	f := newFixture(t)
	ann := f.user("Ann")
	f.put(ann, ChatGPTSubscription, chatgptAuth)
	first, second := f.job(ann, ann, BackendCodex), f.job(ann, ann, BackendCodex)
	c := f.choose(first)
	if c.Kind != ChatGPTSubscription || !c.Exclusive || c.Credential.Home == nil {
		t.Fatalf("first: %+v", c)
	}
	if _, err := f.q.LeaseJob(f.ctx, db.LeaseJobParams{ID: first.ID, LeaseUntil: pgtype.Timestamptz{}, CredentialID: c.ID,
		CredentialKind: c.Kind, CredentialExclusive: true, CredentialVersion: c.Version}); err != nil {
		t.Fatal(err)
	}
	if c2 := f.choose(second); !strings.Contains(c2.Wait, "in use by another job") {
		t.Fatalf("second: %+v", c2)
	}
	// A second exclusive lease of the same login cannot happen even past the check.
	if _, err := f.q.LeaseJob(f.ctx, db.LeaseJobParams{ID: second.ID, CredentialID: c.ID, CredentialKind: c.Kind, CredentialExclusive: true}); err == nil {
		t.Fatal("two active jobs hold one ChatGPT login")
	}
	// With an OpenAI key too, the second runs on that instead of waiting.
	f.put(ann, OpenAIAPIKey, openaiKey)
	if c2 := f.choose(second); c2.Kind != OpenAIAPIKey {
		t.Fatalf("second with a key: %+v", c2)
	}

	leased, _ := f.q.GetJob(f.ctx, first.ID)
	refreshed := strings.Replace(chatgptAuth, "rt_refresh_0123456789abcdef", "rt_refresh_rotated_987654321", 1)
	if err := f.svc.Refreshed(f.ctx, leased, proto.CredentialRefresh{ID: c.Credential.ID, Files: map[string][]byte{"auth.json": []byte(refreshed)}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(f.svc.Sensitive(f.ctx, leased), " "), "rt_refresh_rotated_987654321") {
		t.Fatal("the rotated login was not kept")
	}
	// What a job sends back of a login it was not leased is not kept.
	other, _ := f.q.GetJob(f.ctx, second.ID)
	if err := f.svc.Refreshed(f.ctx, other, proto.CredentialRefresh{ID: c.Credential.ID, Files: map[string][]byte{"auth.json": []byte(chatgptAuth)}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(f.svc.Sensitive(f.ctx, leased), " "), "rt_refresh_rotated_987654321") {
		t.Fatal("another job's report replaced the login")
	}

	// A login the backend turned down is marked, its owner told, and the next job waits.
	if marked, err := f.svc.Refused(f.ctx, leased, "refresh_token_reused"); err != nil || !marked {
		t.Fatal(marked, err)
	}
	list, _ := f.svc.List(f.ctx, ann)
	for _, v := range list {
		if v.Kind == ChatGPTSubscription && (v.Status != StatusError || !strings.Contains(v.LastError, "refresh_token_reused")) {
			t.Fatalf("not marked: %+v", v)
		}
	}
	var told int
	_ = f.pool.QueryRow(f.ctx, "SELECT count(*) FROM events WHERE type = 'credential.refused' AND aggregate_id = $1", ann).Scan(&told)
	if told != 1 {
		t.Fatalf("told %d times", told)
	}
}

// The payer rules: an automation's work is paid for by who switched it on; a turn an agent asked
// for, by the person the chain acts for.
func TestPayerRules(t *testing.T) {
	f := newFixture(t)
	ann, bob := f.user("Ann"), f.user("Bob")
	key := "pr_review"
	if _, err := f.pool.Exec(f.ctx, `INSERT INTO project_automations (project_id, key, enabled, enabled_by, enabled_at) VALUES ($1, $2, true, $3, now())`,
		f.project, key, bob); err != nil {
		t.Fatal(err)
	}
	task, err := f.proc.Create(f.ctx, process.CreateParams{ProjectID: f.project, Kind: "bug", Title: "PR",
		Automation: &process.Automated{Key: key, Title: "PR review", AllowAgents: true, AllowedBy: bob}}, process.Actor{Kind: process.ActorSystem})
	if err != nil {
		t.Fatal(err)
	}
	payers := runners.DefaultPayers()
	who, err := payers.Payer(f.ctx, f.q, runners.PayerQuestion{Task: task, Kind: runners.KindPhase, CreatedBy: process.Actor{Kind: process.ActorSystem}})
	if err != nil || who != bob {
		t.Fatalf("automation: %v %v", who, err)
	}
	who, _ = payers.Payer(f.ctx, f.q, runners.PayerQuestion{Task: task, CreatedBy: process.Actor{Kind: process.ActorUser, ID: ann}})
	if who != ann {
		t.Fatalf("a person who asks pays: %v", who)
	}
	who, _ = payers.Payer(f.ctx, f.q, runners.PayerQuestion{Task: task, PaidBy: ann, CreatedBy: process.Actor{Kind: process.ActorUser, ID: bob}})
	if who != ann {
		t.Fatalf("a named payer: %v", who)
	}
}

// A refresh from a job replaces only the login it was leased: one its owner saved since stays.
func TestARefreshDoesNotOverwriteALoginSavedSince(t *testing.T) {
	f := newFixture(t)
	ann := f.user("Ann")
	f.put(ann, ChatGPTSubscription, chatgptAuth)
	job := f.job(ann, ann, BackendCodex)
	c := f.choose(job)
	leased, err := f.q.LeaseJob(f.ctx, db.LeaseJobParams{ID: job.ID, CredentialID: c.ID, CredentialKind: c.Kind,
		CredentialExclusive: true, CredentialVersion: c.Version})
	if err != nil {
		t.Fatal(err)
	}
	saved := strings.Replace(chatgptAuth, "rt_refresh_0123456789abcdef", "rt_saved_by_ann_0123456789", 1)
	f.put(ann, ChatGPTSubscription, saved) // Ann signs in again while the job runs
	rotated := strings.Replace(chatgptAuth, "rt_refresh_0123456789abcdef", "rt_rotated_by_job_987654321", 1)
	if err := f.svc.Refreshed(f.ctx, leased, proto.CredentialRefresh{ID: c.Credential.ID, Files: map[string][]byte{"auth.json": []byte(rotated)}}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.svc.Sensitive(f.ctx, leased), " "); !strings.Contains(got, "rt_saved_by_ann") || strings.Contains(got, "rt_rotated_by_job") {
		t.Fatalf("the job's refresh replaced Ann's new login: %s", got)
	}
	// A key Codex keeps in a ChatGPT login is masked too.
	k, _ := f.svc.kinds.Get(ChatGPTSubscription)
	withKey := strings.Replace(chatgptAuth, `"OPENAI_API_KEY": null`, `"OPENAI_API_KEY": "sk-proj-made-by-codex-0123456789"`, 1)
	if got := strings.Join(k.Sensitive(withKey), " "); !strings.Contains(got, "sk-proj-made-by-codex-0123456789") {
		t.Fatalf("sensitive: %s", got)
	}
}

// A job saying its login was turned down is a reason to check, not proof: a login its provider
// still takes is not marked; one it refuses is.
func TestARefusalIsConfirmedByACheck(t *testing.T) {
	f := newFixture(t)
	ann := f.user("Ann")
	f.put(ann, AnthropicAPIKey, anthropicKy)
	job := f.job(ann, ann, BackendClaude)
	c := f.choose(job)
	leased, err := f.q.LeaseJob(f.ctx, db.LeaseJobParams{ID: job.ID, CredentialID: c.ID, CredentialKind: c.Kind, CredentialVersion: c.Version})
	if err != nil {
		t.Fatal(err)
	}
	if marked, err := f.svc.Refused(f.ctx, leased, "Invalid API key"); err != nil || marked {
		t.Fatalf("a working key was marked: %v %v", marked, err)
	}
	f.status = http.StatusUnauthorized
	if marked, err := f.svc.Refused(f.ctx, leased, "Invalid API key"); err != nil || !marked {
		t.Fatalf("a refused key was not marked: %v %v", marked, err)
	}
}

// The models endpoint may not take a subscription's token at all: its no leaves the login
// unchecked, with the reason, rather than refused.
func TestASubscriptionsCheckCannotRefuseIt(t *testing.T) {
	f := newFixture(t)
	ann := f.user("Ann")
	f.put(ann, ClaudeSubscription, claudeToken)
	f.status = http.StatusUnauthorized
	v, err := f.svc.Check(f.ctx, ann, ClaudeSubscription)
	if err != nil || v.Status != StatusUnchecked || !strings.Contains(v.LastError, "cannot tell") || v.CheckedAt == nil {
		t.Fatalf("%+v %v", v, err)
	}
}

// A turn nobody in the workspace asked for — from Slack, GitHub, A2A — has no payer, whoever
// created its task: an outsider never spends a member's login.
func TestAnOutsidersTurnHasNoPayer(t *testing.T) {
	f := newFixture(t)
	ann := f.user("Ann")
	task, err := f.proc.Create(f.ctx, process.CreateParams{ProjectID: f.project, Kind: "bug", Title: "Thread"}, process.Actor{Kind: process.ActorUser, ID: ann})
	if err != nil {
		t.Fatal(err)
	}
	var turn pgtype.UUID
	if err := f.pool.QueryRow(f.ctx, `WITH a AS (INSERT INTO agents (project_id, handle, display_name) VALUES ($1, 'helper-'||substr(md5(random()::text), 1, 6), 'H') RETURNING id),
		c AS (INSERT INTO conversations (project_id, kind, name, visibility, created_by_kind) VALUES ($1, 'channel', 'out-'||substr(md5(random()::text), 1, 6), 'project', 'system') RETURNING id),
		m AS (INSERT INTO messages (conversation_id, seq, author_kind, kind, body, origin, status) SELECT id, 1, 'system', 'text', 'hi', '{}', 'final' FROM c RETURNING id, conversation_id)
		INSERT INTO agent_turns (agent_id, project_id, conversation_id, trigger_message_id, requested_by_kind, tier, status, chain_id)
		SELECT a.id, $1, m.conversation_id, m.id, 'external', 'chat', 'queued', gen_random_uuid() FROM a, m RETURNING id`, f.project).Scan(&turn); err != nil {
		t.Fatal(err)
	}
	who, err := runners.DefaultPayers().Payer(f.ctx, f.q, runners.PayerQuestion{Task: task, Kind: runners.KindReply,
		CreatedBy: process.Actor{Kind: process.ActorSystem}, Turn: turn})
	if err != nil || who.Valid {
		t.Fatalf("an outsider's turn is paid by %v (%v)", who, err)
	}
}

// A waiting job names its payer to everyone who reads it by their display name, never by their
// email address; and changing the workspace's rule is recorded with who changed it.
func TestAWaitingJobNamesItsPayerWithoutTheirEmail(t *testing.T) {
	f := newFixture(t)
	var nameless pgtype.UUID
	email := "quiet-" + suffix() + "@t.local"
	if err := f.pool.QueryRow(f.ctx, "INSERT INTO users(email,name,workspace_role) VALUES($1,'','member') RETURNING id", email).Scan(&nameless); err != nil {
		t.Fatal(err)
	}
	j := f.job(nameless, nameless, BackendClaude)
	if strings.Contains(j.WaitingReason, email) || !strings.Contains(j.WaitingReason, "The person who pays for this job") {
		t.Fatalf("reason %q", j.WaitingReason)
	}
	admin := f.user("Admin")
	if err := f.svc.SetRunnerLogins(f.ctx, true, admin); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = f.pool.QueryRow(f.ctx, "SELECT count(*) FROM events WHERE type = 'workspace.runner_logins_changed' AND payload->>'by' = $1", uuidString(admin)).Scan(&n)
	if n != 1 {
		t.Fatalf("recorded %d times", n)
	}
}

// A login's ciphertext is bound to its owner and kind: copied into another person's row it does
// not open, and their job waits instead of running on it.
func TestALoginCopiedToAnotherRowDoesNotOpen(t *testing.T) {
	f := newFixture(t)
	ann, bob := f.user("Ann"), f.user("Bob")
	f.put(ann, ClaudeSubscription, claudeToken)
	f.put(bob, ClaudeSubscription, strings.Replace(claudeToken, "wxyz", "bobs", 1))
	if _, err := f.pool.Exec(f.ctx, `UPDATE user_credentials b SET secret = a.secret FROM user_credentials a
		WHERE a.user_id = $1 AND b.user_id = $2 AND a.kind = b.kind`, ann, bob); err != nil {
		t.Fatal(err)
	}
	c := f.choose(f.job(bob, bob, BackendClaude))
	if c.Credential != nil || c.Wait == "" {
		t.Fatalf("Bob's job ran on Ann's login: %+v", c)
	}
}
