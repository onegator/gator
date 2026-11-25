package credentials

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Probe tries a login with one small real call.
type Probe interface {
	Check(ctx context.Context, token string) error
}

// HTTPProbe asks a provider for something every valid login may read (its list of models) and
// reads the answer as yes (2xx), no (401, 403: ErrRefused) or cannot tell (ErrUnavailable).
type HTTPProbe struct {
	Client   *http.Client
	URL      string
	Provider string // "Anthropic", for the message
	Auth     func(token string, h http.Header)
}

// Check implements Probe. Its errors name the provider and the status, never the token.
func (p HTTPProbe) Check(ctx context.Context, token string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.URL, nil)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrUnavailable, p.Provider)
	}
	p.Auth(token, req.Header)
	res, err := p.Client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s did not answer", ErrUnavailable, p.Provider)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	switch {
	case res.StatusCode >= 200 && res.StatusCode < 300:
		return nil
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: %s turned this login down (%d)", ErrRefused, p.Provider, res.StatusCode)
	default:
		return fmt.Errorf("%w: %s answered %d", ErrUnavailable, p.Provider, res.StatusCode)
	}
}
