package credentials

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/onegator/gator/internal/proto"
)

// The kinds Gator ships, by name.
const (
	ClaudeSubscription  = "claude_subscription"
	AnthropicAPIKey     = "anthropic_api_key"
	ChatGPTSubscription = "chatgpt_subscription"
	OpenAIAPIKey        = "openai_api_key"
)

// Backends the shipped kinds run.
const (
	BackendClaude = "claude"
	BackendCodex  = "acp:codex"
)

// Endpoints are where the kinds' checks call: the providers' APIs, or a fake in tests.
type Endpoints struct {
	Anthropic string // default https://api.anthropic.com
	OpenAI    string // default https://api.openai.com
	HTTP      *http.Client
}

func (e Endpoints) withDefaults() Endpoints {
	if e.Anthropic == "" {
		e.Anthropic = "https://api.anthropic.com"
	}
	if e.OpenAI == "" {
		e.OpenAI = "https://api.openai.com"
	}
	if e.HTTP == nil {
		e.HTTP = http.DefaultClient
	}
	e.Anthropic, e.OpenAI = strings.TrimRight(e.Anthropic, "/"), strings.TrimRight(e.OpenAI, "/")
	return e
}

// DefaultKinds are the shipped kinds, each backend's preferred first: a subscription before an API
// key, because a subscription's limits are already paid for and an API key bills every token.
func DefaultKinds(e Endpoints) *Registry {
	e = e.withDefaults()
	anthropic := func(auth func(token string, h http.Header)) Probe {
		return HTTPProbe{Client: e.HTTP, URL: e.Anthropic + "/v1/models?limit=1", Provider: "Anthropic", Auth: func(token string, h http.Header) {
			h.Set("anthropic-version", "2023-06-01")
			auth(token, h)
		}}
	}
	return NewRegistry(
		tokenKind{
			name: ClaudeSubscription, backend: BackendClaude, prefix: "sk-ant-oat", minLen: 40,
			desc: Description{Label: "Claude subscription",
				Help: "Run `claude setup-token` on your computer, sign in with your Claude account, and paste the token it prints (sk-ant-oat…)."},
			env: []string{"CLAUDE_CODE_OAUTH_TOKEN"},
			probe: anthropic(func(token string, h http.Header) {
				h.Set("Authorization", "Bearer "+token)
				h.Set("anthropic-beta", "oauth-2025-04-20")
			}),
		},
		tokenKind{
			name: AnthropicAPIKey, backend: BackendClaude, prefix: "sk-ant-api", minLen: 40,
			desc: Description{Label: "Anthropic API key",
				Help: "Create a key at console.anthropic.com → API keys and paste it (sk-ant-api…). Every token is billed to that key's organisation."},
			env:   []string{"ANTHROPIC_API_KEY"},
			probe: anthropic(func(token string, h http.Header) { h.Set("x-api-key", token) }),
		},
		codexLogin{},
		tokenKind{
			name: OpenAIAPIKey, backend: BackendCodex, prefix: "sk-", notPrefix: "sk-ant-", minLen: 20,
			desc: Description{Label: "OpenAI API key",
				Help: "Create a key at platform.openai.com → API keys and paste it (sk-…). Every token is billed to that key's project."},
			env: []string{"OPENAI_API_KEY", "CODEX_API_KEY"},
			// Codex keeps an API-key login in its auth.json too; a home of the job's own keeps the
			// runner's ~/.codex (somebody else's login, perhaps) out of it.
			codexHome: true,
			probe: HTTPProbe{Client: e.HTTP, URL: e.OpenAI + "/v1/models", Provider: "OpenAI",
				Auth: func(token string, h http.Header) { h.Set("Authorization", "Bearer "+token) }},
		},
	)
}

// tokenKind is a login that is one token: what a person pastes is what the agent's CLI reads from
// its environment.
type tokenKind struct {
	name, backend     string
	desc              Description
	prefix, notPrefix string
	minLen            int
	env               []string
	codexHome         bool
	probe             Probe
}

func (k tokenKind) Name() string          { return k.name }
func (k tokenKind) Backend() string       { return k.backend }
func (k tokenKind) Describe() Description { return k.desc }

func (k tokenKind) Parse(raw string) (Secret, error) {
	v := strings.TrimSpace(raw)
	switch {
	case v == "":
		return Secret{}, invalid("paste the %s", k.desc.Label)
	case !strings.HasPrefix(v, k.prefix) || (k.notPrefix != "" && strings.HasPrefix(v, k.notPrefix)):
		return Secret{}, invalid("that is not a %s: one starts with %s", k.desc.Label, k.prefix)
	case len(v) < k.minLen || len(v) > 512:
		return Secret{}, invalid("that is not a %s: it is %d characters long", k.desc.Label, len(v))
	case strings.IndexFunc(v, func(r rune) bool { return !tokenRune(r) }) >= 0:
		return Secret{}, invalid("that is not a %s: it holds characters a token does not", k.desc.Label)
	}
	return Secret{Value: v, Hint: hint(v)}, nil
}

func tokenRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
}

func (k tokenKind) Lease(secret string) proto.Credential {
	c := proto.Credential{Kind: k.name, Env: map[string]string{}, Redact: []string{secret}}
	for _, e := range k.env {
		c.Env[e] = secret
	}
	if k.codexHome {
		auth, _ := json.Marshal(map[string]string{"OPENAI_API_KEY": secret})
		c.Home = &proto.CredentialHome{Env: "CODEX_HOME", Files: map[string][]byte{codexAuthFile: auth}}
	}
	return c
}

func (k tokenKind) Sensitive(secret string) []string { return []string{secret} }

// Check implements Checker.
func (k tokenKind) Check(ctx context.Context, secret string) error {
	if k.probe == nil {
		return ErrCannotCheck
	}
	return k.probe.Check(ctx, secret)
}

// codexAuthFile is where Codex keeps its login, under CODEX_HOME.
const codexAuthFile = "auth.json"

// codexLogin is a ChatGPT login for Codex: the auth.json `codex login` writes, whose access token
// Codex refreshes as it works, replacing the refresh token each time. It is held by one job at a
// time and what the job leaves in auth.json is kept.
type codexLogin struct{}

var _ Rotating = codexLogin{}

func (codexLogin) Name() string    { return ChatGPTSubscription }
func (codexLogin) Backend() string { return BackendCodex }
func (codexLogin) Describe() Description {
	return Description{Label: "ChatGPT subscription",
		Help: "Run `codex login` on your computer, sign in with your ChatGPT account, and paste the contents of ~/.codex/auth.json. " +
			"Gator keeps it up to date as Codex refreshes it; one job uses it at a time.", Multiline: true}
}

// codexAuth is what Gator reads of auth.json; the file is kept as it was written.
type codexAuth struct {
	APIKey *string `json:"OPENAI_API_KEY"`
	Tokens *struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
}

func (k codexLogin) Parse(raw string) (Secret, error) {
	v := strings.TrimSpace(raw)
	if v == "" {
		return Secret{}, invalid("paste the contents of ~/.codex/auth.json")
	}
	if len(v) > 64<<10 {
		return Secret{}, invalid("that is not a Codex auth.json: it is %d bytes long", len(v))
	}
	a, err := readCodexAuth([]byte(v))
	if err != nil {
		return Secret{}, err
	}
	var compact bytes.Buffer
	_ = json.Compact(&compact, []byte(v))
	h := a.Tokens.AccountID
	if h == "" {
		h = a.Tokens.RefreshToken
	}
	return Secret{Value: compact.String(), Hint: hint(h)}, nil
}

func readCodexAuth(b []byte) (codexAuth, error) {
	var a codexAuth
	// The decoder's own message can quote the input; this one does not.
	if err := json.Unmarshal(b, &a); err != nil {
		return a, invalid("that is not a Codex auth.json: it is not a JSON object")
	}
	if a.APIKey != nil && *a.APIKey != "" && a.Tokens == nil {
		return a, invalid("that auth.json holds an API key, not a ChatGPT login: save it as an OpenAI API key")
	}
	if a.Tokens == nil || a.Tokens.AccessToken == "" || a.Tokens.RefreshToken == "" {
		return a, invalid("that auth.json has no ChatGPT login in it: run `codex login` and choose to sign in with ChatGPT")
	}
	return a, nil
}

func (k codexLogin) Lease(secret string) proto.Credential {
	return proto.Credential{Kind: k.Name(), Redact: k.Sensitive(secret), Home: &proto.CredentialHome{Env: "CODEX_HOME",
		Files: map[string][]byte{codexAuthFile: []byte(secret)}, Refresh: []string{codexAuthFile}}}
}

func (k codexLogin) Sensitive(secret string) []string {
	a, err := readCodexAuth([]byte(secret))
	if err != nil {
		return []string{secret}
	}
	out := []string{a.Tokens.AccessToken, a.Tokens.RefreshToken}
	if a.Tokens.IDToken != "" {
		out = append(out, a.Tokens.IDToken)
	}
	if a.APIKey != nil && *a.APIKey != "" {
		out = append(out, *a.APIKey) // a ChatGPT login may carry a key Codex made for it
	}
	return out
}

func (k codexLogin) Refreshed(old string, files map[string][]byte) (Secret, error) {
	b, ok := files[codexAuthFile]
	if !ok {
		return k.Parse(old)
	}
	return k.Parse(string(b))
}
