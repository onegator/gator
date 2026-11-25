package mcpremote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
	res, err := t.base.RoundTrip(r)
	if err == nil {
		res.Body = &boundedBody{ReadCloser: res.Body, left: MaxAnswer}
	}
	return res, err
}

// MaxAnswer bounds what one answer of a remote server is read to, before anything parses it.
const MaxAnswer = 4 << 20

// ErrTooLarge is an answer past MaxAnswer.
var ErrTooLarge = errors.New("the server's answer is larger than Gator reads")

// boundedBody reads a response up to a limit and fails past it, rather than cutting it short and
// handing on half a JSON document.
type boundedBody struct {
	io.ReadCloser
	left int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		var one [1]byte
		if n, _ := b.ReadCloser.Read(one[:]); n > 0 {
			return 0, ErrTooLarge
		}
		return 0, io.EOF
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.ReadCloser.Read(p)
	b.left -= int64(n)
	return n, err
}
