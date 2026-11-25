package mcpremote

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/onegator/gator/internal/server/secrets"
	"github.com/onegator/gator/internal/server/store/db"
)

// Credentials put a server's credential on a request to it. Only the proxy's own requests carry
// it; nothing a runner sends or receives does.
type Credentials interface {
	Authorize(ctx context.Context, srv db.McpServer, h http.Header) error
}

// Secret names in a server's secrets.
const (
	SecretToken        = "token"         // auth: bearer
	SecretAccessToken  = "access_token"  // auth: oauth
	SecretRefreshToken = "refresh_token" // auth: oauth, never sent anywhere but the provider
)

// Keyring reads servers' credentials from their secrets, encrypted with the server's keyring.
type Keyring struct {
	Keys *secrets.Keyring
}

// Authorize implements Credentials.
func (k Keyring) Authorize(_ context.Context, srv db.McpServer, h http.Header) error {
	if srv.Auth == "none" {
		return nil
	}
	sec, err := k.Secrets(srv)
	if err != nil {
		return err
	}
	token := sec[SecretToken]
	if srv.Auth == "oauth" {
		token = sec[SecretAccessToken]
	}
	if token == "" {
		return ErrNotConnected
	}
	h.Set("Authorization", "Bearer "+token)
	return nil
}

// Secrets decrypts a server's secrets.
func (k Keyring) Secrets(srv db.McpServer) (map[string]string, error) {
	out := map[string]string{}
	if srv.Secrets == "" {
		return out, nil
	}
	if k.Keys == nil {
		return nil, ErrNoKeyring
	}
	plain, err := k.Keys.Decrypt(srv.Secrets)
	if err != nil {
		return nil, err
	}
	return out, json.Unmarshal(plain, &out)
}

// Seal encrypts secrets for a server's row.
func (k Keyring) Seal(sec map[string]string) (string, error) {
	if len(sec) == 0 {
		return "", nil
	}
	if k.Keys == nil {
		return "", ErrNoKeyring
	}
	b, _ := json.Marshal(sec)
	return k.Keys.Encrypt(b)
}

// credentialTransport is the HTTP transport of one server's requests: its credential on each.
type credentialTransport struct {
	base  http.RoundTripper
	srv   db.McpServer
	creds Credentials
}

func (t credentialTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if t.creds != nil {
		if err := t.creds.Authorize(r.Context(), t.srv, r.Header); err != nil {
			return nil, err
		}
	}
	return t.base.RoundTrip(r)
}
