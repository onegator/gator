package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	claudeToken = "sk-ant-oat01-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-wxyz"
	anthropicKy = "sk-ant-api03-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-key1"
	openaiKey   = "sk-proj-AbCdEfGhIjKlMnOpQrStUv0123456789open"
	chatgptAuth = `{"OPENAI_API_KEY": null, "tokens": {"id_token": "eyJid.token.value-0123456789", "access_token": "eyJaccess.token.value-0123456789",
		"refresh_token": "rt_refresh_0123456789abcdef", "account_id": "acct-1234-abcd"}, "last_refresh": "2026-11-01T00:00:00Z"}`
)

func TestShippedKindsReadWhatPeoplePaste(t *testing.T) {
	r := DefaultKinds(Endpoints{})
	cases := []struct {
		kind, raw, hint string
	}{
		{ClaudeSubscription, "  " + claudeToken + "\n", "…wxyz"},
		{AnthropicAPIKey, anthropicKy, "…key1"},
		{OpenAIAPIKey, openaiKey, "…open"},
		{ChatGPTSubscription, chatgptAuth, "…abcd"},
	}
	for _, c := range cases {
		k, ok := r.Get(c.kind)
		if !ok {
			t.Fatalf("%s is not registered", c.kind)
		}
		sec, err := k.Parse(c.raw)
		if err != nil {
			t.Fatalf("%s: %v", c.kind, err)
		}
		if sec.Hint != c.hint {
			t.Errorf("%s: hint %q, want %q", c.kind, sec.Hint, c.hint)
		}
	}
}

// What a person pasted wrong is refused by its shape, and the refusal never quotes it.
func TestMalformedLoginsAreRefusedWithoutBeingQuoted(t *testing.T) {
	r := DefaultKinds(Endpoints{})
	cases := map[string][]string{
		ClaudeSubscription:  {"", anthropicKy, "sk-ant-oat01-short", claudeToken + " extra", "sk-ant-oat01-" + strings.Repeat("x", 30) + "$"},
		AnthropicAPIKey:     {claudeToken, openaiKey, "sk-ant-api03-short"},
		OpenAIAPIKey:        {anthropicKy, "pk-" + strings.Repeat("a", 30), "sk-short"},
		ChatGPTSubscription: {"not json sk-secret-in-garbage", `{"OPENAI_API_KEY":"sk-proj-0123456789abcdefghij"}`, `{"tokens":{"access_token":"a"}}`},
	}
	for kind, raws := range cases {
		k, _ := r.Get(kind)
		for _, raw := range raws {
			_, err := k.Parse(raw)
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("%s accepted %q: %v", kind, raw, err)
				continue
			}
			if raw != "" && len(raw) > 8 && strings.Contains(err.Error(), strings.TrimSpace(raw)) {
				t.Errorf("%s's refusal quotes the value: %v", kind, err)
			}
			if strings.Contains(err.Error(), "sk-secret") {
				t.Errorf("%s's refusal quotes the value: %v", kind, err)
			}
		}
	}
}

// Each backend prefers a subscription to an API key; nothing else in Gator knows the order.
func TestBackendsPreferSubscriptions(t *testing.T) {
	r := DefaultKinds(Endpoints{})
	names := func(ks []Kind) (out []string) {
		for _, k := range ks {
			out = append(out, k.Name())
		}
		return out
	}
	if got := strings.Join(names(r.ForBackend(BackendClaude)), ","); got != "claude_subscription,anthropic_api_key" {
		t.Errorf("claude: %s", got)
	}
	if got := strings.Join(names(r.ForBackend(BackendCodex)), ","); got != "chatgpt_subscription,openai_api_key" {
		t.Errorf("codex: %s", got)
	}
	if len(r.ForBackend("acp:gemini")) != 0 {
		t.Error("gemini has kinds")
	}
	defer func() {
		if recover() == nil {
			t.Error("a kind registered twice")
		}
	}()
	r.Register(codexLogin{})
}

// What a job's agent is handed: the variables each CLI reads, and for Codex a home of the job's
// own whose auth.json comes back when Codex rewrites it.
func TestLeasesGiveEachCLIItsLogin(t *testing.T) {
	r := DefaultKinds(Endpoints{})
	k, _ := r.Get(ClaudeSubscription)
	if c := k.Lease(claudeToken); c.Env["CLAUDE_CODE_OAUTH_TOKEN"] != claudeToken || c.Home != nil || c.Redact[0] != claudeToken {
		t.Errorf("claude subscription: %+v", c)
	}
	k, _ = r.Get(AnthropicAPIKey)
	if c := k.Lease(anthropicKy); c.Env["ANTHROPIC_API_KEY"] != anthropicKy || len(c.Env) != 1 {
		t.Errorf("anthropic key: %+v", c)
	}
	k, _ = r.Get(OpenAIAPIKey)
	c := k.Lease(openaiKey)
	var auth map[string]string
	_ = json.Unmarshal(c.Home.Files["auth.json"], &auth)
	if c.Env["OPENAI_API_KEY"] != openaiKey || c.Home.Env != "CODEX_HOME" || auth["OPENAI_API_KEY"] != openaiKey || len(c.Home.Refresh) != 0 {
		t.Errorf("openai key: %+v", c)
	}
	k, _ = r.Get(ChatGPTSubscription)
	sec, _ := k.Parse(chatgptAuth)
	c = k.Lease(sec.Value)
	if c.Home == nil || c.Home.Env != "CODEX_HOME" || string(c.Home.Files["auth.json"]) != sec.Value || c.Home.Refresh[0] != "auth.json" || len(c.Env) != 0 {
		t.Errorf("chatgpt: %+v", c)
	}
	if got := strings.Join(c.Redact, " "); !strings.Contains(got, "rt_refresh_0123456789abcdef") || !strings.Contains(got, "eyJaccess") {
		t.Errorf("chatgpt redacts %q", got)
	}
	if _, ok := k.(Rotating); !ok {
		t.Error("a ChatGPT login does not rotate")
	}
}

// A check is one small call to the provider: yes, no (refused), or cannot tell.
func TestChecksReadTheProvidersAnswer(t *testing.T) {
	var got http.Header
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(status)
	}))
	defer srv.Close()
	r := DefaultKinds(Endpoints{Anthropic: srv.URL, OpenAI: srv.URL, HTTP: srv.Client()})
	k, _ := r.Get(AnthropicAPIKey)
	checker := k.(Checker)
	if err := checker.Check(context.Background(), anthropicKy); err != nil || got.Get("x-api-key") != anthropicKy {
		t.Fatalf("ok: %v %v", err, got)
	}
	status = http.StatusUnauthorized
	err := checker.Check(context.Background(), anthropicKy)
	if !errors.Is(err, ErrRefused) || strings.Contains(err.Error(), anthropicKy) {
		t.Fatalf("401: %v", err)
	}
	status = http.StatusBadGateway
	if err := checker.Check(context.Background(), anthropicKy); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("502: %v", err)
	}
	status = http.StatusOK
	k, _ = r.Get(ClaudeSubscription)
	if err := k.(Checker).Check(context.Background(), claudeToken); err != nil || got.Get("Authorization") != "Bearer "+claudeToken {
		t.Fatalf("oauth: %v %v", err, got)
	}
	k, _ = r.Get(ChatGPTSubscription)
	if _, ok := k.(Checker); ok {
		t.Error("a ChatGPT login says it can be checked; checking would spend its refresh token")
	}
}
