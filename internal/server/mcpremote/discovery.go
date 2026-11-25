package mcpremote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// The MCP authorization spec, as the server follows it to connect a remote server: the server says
// which authorization server protects it (protected resource metadata, RFC 9728), that server says
// where its endpoints are (authorization server metadata, RFC 8414), and Gator registers a client
// of its own there (dynamic client registration, RFC 7591), signing in with PKCE. Every endpoint
// comes from that chain — never from a package — so a client registered for one server, or the
// code a person signs in with, only ever goes to the authorization server that server names.

// maxMetadata bounds a metadata or registration answer.
const maxMetadata = 64 << 10

// resourceMetadata is RFC 9728's document.
type resourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

// serverMetadata is RFC 8414's document, the fields Gator uses.
type serverMetadata struct {
	Issuer                        string   `json:"issuer"`
	AuthorizationEndpoint         string   `json:"authorization_endpoint"`
	TokenEndpoint                 string   `json:"token_endpoint"`
	RegistrationEndpoint          string   `json:"registration_endpoint"`
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
}

// Discovered is what discovery found for one server, kept in mcp_servers.oauth beside what its
// package declared.
type Discovered struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint,omitempty"`
	Resource              string `json:"resource"`
}

var resourceMetadataParam = regexp.MustCompile(`resource_metadata="([^"]+)"`)

// discover follows a server's metadata to its authorization server. want is the authorization
// server its package declared: one the server names that is not it is refused, so what a person
// accepted ("signs in at <host>") is where they sign in.
func discover(ctx context.Context, hc *http.Client, serverURL, want string) (Discovered, error) {
	prm, err := protectedResource(ctx, hc, serverURL)
	if err != nil {
		return Discovered{}, err
	}
	if strings.TrimRight(prm.Resource, "/") != strings.TrimRight(serverURL, "/") {
		return Discovered{}, fmt.Errorf("%w: the server's metadata is for %q, not %q", ErrNotConnectable, prm.Resource, serverURL)
	}
	issuer := ""
	for _, as := range prm.AuthorizationServers {
		if want == "" || sameOrigin(as, want) {
			issuer = as
			break
		}
	}
	if issuer == "" {
		return Discovered{}, fmt.Errorf("%w: the server names %v as its authorization server, its package %s", ErrNotConnectable, prm.AuthorizationServers, want)
	}
	asm, err := authorizationServer(ctx, hc, issuer)
	if err != nil {
		return Discovered{}, err
	}
	if !slices.Contains(asm.CodeChallengeMethodsSupported, "S256") {
		return Discovered{}, fmt.Errorf("%w: %s does not offer PKCE (S256)", ErrNotConnectable, issuer)
	}
	for _, e := range []string{asm.AuthorizationEndpoint, asm.TokenEndpoint, asm.RegistrationEndpoint} {
		if e != "" && !sameOrigin(e, issuer) {
			// An authorization server's endpoints live on it; one elsewhere would carry a client's
			// secret and a person's code off to a third host.
			return Discovered{}, fmt.Errorf("%w: %s keeps an endpoint on another host (%s)", ErrNotConnectable, issuer, hostOf(e))
		}
	}
	if asm.AuthorizationEndpoint == "" || asm.TokenEndpoint == "" {
		return Discovered{}, fmt.Errorf("%w: %s names no authorization or token endpoint", ErrNotConnectable, issuer)
	}
	return Discovered{Issuer: asm.Issuer, AuthorizationEndpoint: asm.AuthorizationEndpoint, TokenEndpoint: asm.TokenEndpoint,
		RegistrationEndpoint: asm.RegistrationEndpoint, Resource: prm.Resource}, nil
}

// protectedResource reads RFC 9728's document: from where a 401 from the server says it is, else
// from the well-known address with the server's path, then without it.
func protectedResource(ctx context.Context, hc *http.Client, serverURL string) (resourceMetadata, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return resourceMetadata{}, err
	}
	var candidates []string
	if res, err := post(ctx, hc, serverURL, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)); err == nil {
		if res.StatusCode == http.StatusUnauthorized {
			if m := resourceMetadataParam.FindStringSubmatch(res.Header.Get("WWW-Authenticate")); m != nil && sameOrigin(m[1], serverURL) {
				candidates = append(candidates, m[1])
			}
		}
		res.Body.Close()
	}
	base := u.Scheme + "://" + u.Host + "/.well-known/oauth-protected-resource"
	if p := strings.TrimRight(u.Path, "/"); p != "" {
		candidates = append(candidates, base+p)
	}
	candidates = append(candidates, base)
	var last error
	for _, c := range candidates {
		var prm resourceMetadata
		if last = getJSON(ctx, hc, c, &prm); last == nil && len(prm.AuthorizationServers) > 0 {
			return prm, nil
		}
	}
	return resourceMetadata{}, fmt.Errorf("%w: the server publishes no protected resource metadata (RFC 9728): %v", ErrNotConnectable, last)
}

// authorizationServer reads RFC 8414's document of issuer (or, failing it, OpenID's), and holds it
// to being that issuer's.
func authorizationServer(ctx context.Context, hc *http.Client, issuer string) (serverMetadata, error) {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return serverMetadata{}, fmt.Errorf("%w: %q is not an issuer", ErrNotConnectable, issuer)
	}
	p := strings.TrimRight(u.Path, "/")
	origin := u.Scheme + "://" + u.Host
	var last error
	for _, c := range []string{origin + "/.well-known/oauth-authorization-server" + p, origin + "/.well-known/openid-configuration" + p,
		strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"} {
		var m serverMetadata
		if last = getJSON(ctx, hc, c, &m); last != nil {
			continue
		}
		if strings.TrimRight(m.Issuer, "/") != strings.TrimRight(issuer, "/") {
			last = fmt.Errorf("the metadata is %s's, not %s's", m.Issuer, issuer)
			continue
		}
		return m, nil
	}
	return serverMetadata{}, fmt.Errorf("%w: %s publishes no authorization server metadata: %v", ErrNotConnectable, issuer, last)
}

// registered is a client a registration made.
type registered struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret,omitempty"`
}

// register makes Gator a client of an authorization server for one server (RFC 7591).
func register(ctx context.Context, hc *http.Client, endpoint, redirect string) (registered, error) {
	body, _ := json.Marshal(map[string]any{"client_name": "Gator", "redirect_uris": []string{redirect},
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
		"token_endpoint_auth_method": "client_secret_post"})
	res, err := post(ctx, hc, endpoint, body)
	if err != nil {
		return registered{}, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, maxMetadata))
	if res.StatusCode != http.StatusCreated && res.StatusCode != http.StatusOK {
		return registered{}, fmt.Errorf("%w: registering a client: %s", ErrNotConnectable, res.Status)
	}
	var c registered
	if err := json.Unmarshal(raw, &c); err != nil || c.ClientID == "" {
		return registered{}, fmt.Errorf("%w: the registration answered no client", ErrNotConnectable)
	}
	return c, nil
}

func getJSON(ctx context.Context, hc *http.Client, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", u, res.Status)
	}
	return json.NewDecoder(io.LimitReader(res.Body, maxMetadata)).Decode(out)
}

func post(ctx context.Context, hc *http.Client, u string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	return hc.Do(req)
}

// sameOrigin says whether two URLs share scheme and host.
func sameOrigin(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	return errA == nil && errB == nil && ua.Host != "" && strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host)
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return raw
}
