package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/backend"
	"github.com/onegator/gator/internal/runner/client"
	"github.com/onegator/gator/internal/runner/workspace"
)

const (
	payerToken   = "sk-ant-oat01-payer-token-0123456789abcdef"
	oldRefresh   = "rt_old_refresh_token_0123456789"
	newRefresh   = "rt_new_refresh_token_9876543210"
	accessToken  = "eyJaccess.token.of-the-payer-0123456789"
	codexAuthOld = `{"tokens":{"access_token":"` + accessToken + `","refresh_token":"` + oldRefresh + `"}}`
	codexAuthNew = `{"tokens":{"access_token":"` + accessToken + `","refresh_token":"` + newRefresh + `"}}`
)

// recordingIO keeps every event's payload as it would go to the server.
func recordingIO() (client.JobIO, func() string) {
	var mu sync.Mutex
	var b strings.Builder
	return client.JobIO{Emit: func(typ string, payload any) {
		raw, _ := json.Marshal(payload)
		mu.Lock()
		b.WriteString(typ + " " + string(raw) + "\n")
		mu.Unlock()
	}}, func() string {
		mu.Lock()
		defer mu.Unlock()
		return b.String()
	}
}

// A job on its payer's Claude login: the token is in the agent's environment, marked so the
// backend drops the runner's own, and nothing the job reports carries it — not its events, not
// its summary, however the agent printed it.
func TestPayersLoginReachesTheAgentAndNothingElse(t *testing.T) {
	be := &scripted{run: func(_ context.Context, _ int, spec backend.Spec, emit backend.Emit) backend.Outcome {
		emit("text", map[string]any{"text": "my token is " + payerToken})
		return backend.Outcome{Status: proto.StatusDone, Summary: "done; I used " + payerToken, Usage: usage()}
	}}
	e := &Executor{WS: &workspace.Manager{Root: t.TempDir()}, Backends: map[string]backend.Backend{"fake": be}}
	io, sent := recordingIO()
	fin := e.Run(context.Background(), proto.Job{JobID: "j1", Backend: "fake", Credential: &proto.Credential{ID: "c1", Kind: "claude_subscription",
		Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": payerToken}, Redact: []string{payerToken}}}, io)
	if fin.Status != proto.StatusDone {
		t.Fatalf("finish %+v", fin)
	}
	spec := be.specs[0]
	if !spec.PayerLogin || !slices.Contains(spec.Env, "CLAUDE_CODE_OAUTH_TOKEN="+payerToken) {
		t.Fatalf("the agent did not get the payer's login: %v %v", spec.PayerLogin, spec.Env)
	}
	if strings.Contains(sent(), payerToken) || strings.Contains(fin.Summary, payerToken) {
		t.Fatalf("the token left the runner:\n%s\n%s", sent(), fin.Summary)
	}
	if !strings.Contains(sent(), "[redacted]") || !strings.Contains(fin.Summary, "[redacted]") {
		t.Fatalf("nothing was masked:\n%s\n%s", sent(), fin.Summary)
	}
}

// A ChatGPT login lives in a CODEX_HOME made for the job (0700, auth.json 0600), which goes when the
// job ends; what Codex rewrote there comes back in the finish.
func TestCodexHomeIsTheJobsOwnAndItsRefreshComesBack(t *testing.T) {
	var home string
	be := &scripted{run: func(_ context.Context, _ int, spec backend.Spec, _ backend.Emit) backend.Outcome {
		for _, kv := range spec.Env {
			if v, ok := strings.CutPrefix(kv, "CODEX_HOME="); ok {
				home = v
			}
		}
		if dir, err := os.Stat(home); err != nil || dir.Mode().Perm() != 0o700 {
			t.Errorf("home %s: %v %v", home, dir, err)
		}
		if f, err := os.Stat(filepath.Join(home, "auth.json")); err != nil || f.Mode().Perm() != 0o600 {
			t.Errorf("auth.json: %v %v", f, err)
		}
		if b, _ := os.ReadFile(filepath.Join(home, "auth.json")); string(b) != codexAuthOld {
			t.Errorf("auth.json holds %s", b)
		}
		_ = os.WriteFile(filepath.Join(home, "auth.json"), []byte(codexAuthNew), 0o600) // codex refreshed it
		return backend.Outcome{Status: proto.StatusDone, Summary: "ok", Usage: usage()}
	}}
	e := &Executor{WS: &workspace.Manager{Root: t.TempDir()}, Backends: map[string]backend.Backend{"fake": be}}
	io, _ := recordingIO()
	fin := e.Run(context.Background(), proto.Job{JobID: "j2", Backend: "fake", Credential: &proto.Credential{ID: "c2", Kind: "chatgpt_subscription",
		Home:   &proto.CredentialHome{Env: "CODEX_HOME", Files: map[string][]byte{"auth.json": []byte(codexAuthOld)}, Refresh: []string{"auth.json"}},
		Redact: []string{accessToken, oldRefresh}}}, io)
	if home == "" {
		t.Fatal("no CODEX_HOME")
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("the job's login outlived it: %v", err)
	}
	r := fin.CredentialRefresh
	if r == nil || r.ID != "c2" || string(r.Files["auth.json"]) != codexAuthNew {
		t.Fatalf("refresh %+v", r)
	}
}

// Nothing rewritten, nothing sent back; and a login the backend turned down is said so, masked.
func TestUnchangedLoginStaysAndARefusalIsReported(t *testing.T) {
	be := &scripted{run: func(context.Context, int, backend.Spec, backend.Emit) backend.Outcome {
		return backend.Outcome{Status: proto.StatusFailed, Reason: "claude success: Invalid API key · Please run /login (" + payerToken + ")"}
	}}
	e := &Executor{WS: &workspace.Manager{Root: t.TempDir()}, Backends: map[string]backend.Backend{"fake": be}}
	io, _ := recordingIO()
	fin := e.Run(context.Background(), proto.Job{JobID: "j3", Backend: "fake", Credential: &proto.Credential{ID: "c3", Kind: "anthropic_api_key",
		Env: map[string]string{"ANTHROPIC_API_KEY": payerToken}, Redact: []string{payerToken},
		Home: &proto.CredentialHome{Env: "CODEX_HOME", Files: map[string][]byte{"auth.json": []byte("{}")}, Refresh: []string{"auth.json"}}}}, io)
	if fin.CredentialRefresh != nil {
		t.Fatalf("an unchanged login was sent back: %+v", fin.CredentialRefresh)
	}
	if !strings.Contains(fin.CredentialRefused, "Invalid API key") || strings.Contains(fin.CredentialRefused+fin.StopReason, payerToken) {
		t.Fatalf("refused %q, reason %q", fin.CredentialRefused, fin.StopReason)
	}
}

// A job on the runner's own login is as it always was; a failure is not a refused login.
func TestRunnersOwnLoginIsUntouched(t *testing.T) {
	be := &scripted{run: func(context.Context, int, backend.Spec, backend.Emit) backend.Outcome {
		return backend.Outcome{Status: proto.StatusFailed, Reason: "Invalid API key"}
	}}
	e := &Executor{WS: &workspace.Manager{Root: t.TempDir()}, Backends: map[string]backend.Backend{"fake": be}}
	io, _ := recordingIO()
	fin := e.Run(context.Background(), proto.Job{JobID: "j4", Backend: "fake"}, io)
	if be.specs[0].PayerLogin || fin.CredentialRefused != "" || fin.CredentialRefresh != nil {
		t.Fatalf("spec %+v finish %+v", be.specs[0], fin)
	}
}

// A server cannot use a login to set what runs the runner or what a process loads.
func TestLoginVariablesAreLoginsOnly(t *testing.T) {
	for _, k := range []string{"PATH", "LD_PRELOAD", "GATOR_RUNNER_TOKEN", "HOME", "lower", "A=B", "NODE_OPTIONS"} {
		if _, err := openLogin(&proto.Credential{Env: map[string]string{k: "x"}}); err == nil {
			t.Errorf("%s was accepted", k)
		}
	}
	if _, err := openLogin(&proto.Credential{Home: &proto.CredentialHome{Env: "CODEX_HOME", Files: map[string][]byte{"../x": nil}}}); err == nil {
		t.Error("a file outside the login's directory was accepted")
	}
}
