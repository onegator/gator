package cli

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// session is what every signed-in command starts from: which server, which token.
type session struct {
	cfg    Config
	server string
	client *Client
}

// serverFor picks the server: --server, then GATOR_SERVER, then the one signed in to.
func serverFor(env *Env, g *globals, cfg Config) (string, error) {
	for _, s := range []string{g.server, env.Getenv("GATOR_SERVER"), cfg.Server} {
		if strings.TrimSpace(s) != "" {
			return NormalizeServer(s)
		}
	}
	return "", coded(exitAuth, "no server yet: sign in with gator login --server https://your-gator")
}

// open resolves the server and the token for a command that needs to be signed in.
func open(env *Env, g *globals) (*session, error) {
	cfg, err := LoadConfig(env.ConfigDir)
	if err != nil {
		return nil, err
	}
	server, err := serverFor(env, g, cfg)
	if err != nil {
		return nil, err
	}
	token := env.Getenv("GATOR_TOKEN")
	if token == "" {
		// A job on a runner on this Mac runs as the same macOS user. Its agent speaks with the
		// job's identity through gator-cli; it must not borrow the person's sign-in.
		if env.Getenv("GATOR_JOB_TOKEN") != "" {
			return nil, coded(exitAuth, "this shell is inside a Gator job; the agent's tool is gator-cli, and gator will not use a person's stored sign-in here")
		}
		token, err = env.Store.Get(server)
		if errors.Is(err, ErrNoToken) {
			return nil, coded(exitAuth, "not signed in to %s: run gator login", server)
		}
		if err != nil {
			return nil, err
		}
	}
	return &session{cfg: cfg, server: server, client: &Client{Base: server, Token: token, HTTP: env.HTTP}}, nil
}

func cmdLogin(ctx context.Context, env *Env, args []string) error {
	fs, g := newFlags("login")
	token := fs.String("token", "", "a token made elsewhere (- reads it from stdin)")
	noBrowser := fs.Bool("no-browser", false, "print the sign-in address instead of opening it")
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(positional) == 1 && g.server == "" {
		g.server = positional[0] // gator login https://gator.example.com
	} else if len(positional) > 0 {
		return coded(exitUserErr, "unexpected arguments: %s", strings.Join(positional, " "))
	}
	cfg, err := LoadConfig(env.ConfigDir)
	if err != nil {
		return err
	}
	server, err := serverFor(env, g, cfg)
	if err != nil {
		return coded(exitUserErr, "which server? gator login --server https://your-gator")
	}
	anon := &Client{Base: server, HTTP: env.HTTP}

	var expires time.Time
	switch {
	case *token != "":
		t := *token
		if t == "-" {
			raw, err := io.ReadAll(io.LimitReader(env.Stdin, 4096))
			if err != nil {
				return coded(exitUserErr, "reading the token from stdin: %v", err)
			}
			t = string(raw)
		}
		*token = strings.TrimSpace(t)
		if *token == "" {
			return coded(exitUserErr, "the token is empty")
		}
	default:
		got, err := browserLogin(ctx, env, anon, !*noBrowser)
		if err != nil {
			return err
		}
		*token, expires = got.Token, got.ExpiresAt
	}

	c := &Client{Base: server, Token: *token, HTTP: env.HTTP}
	me, err := c.Me(ctx)
	if err != nil {
		return fmt.Errorf("the server did not accept the token: %w", err)
	}
	if err := env.Store.Set(server, *token); err != nil {
		return err
	}
	if cfg.Server != server {
		// Another server's default project means nothing here.
		cfg.Project = ""
	}
	cfg.Server = server
	if err := SaveConfig(env.ConfigDir, cfg); err != nil {
		return err
	}
	if g.json {
		return printJSON(env, map[string]any{"server": server, "me": me, "expiresAt": nullableTime(expires)})
	}
	fmt.Fprintf(env.Stdout, "Signed in to %s as %s.\n", server, me.Name)
	where := "Token kept in " + env.Store.Where()
	if !expires.IsZero() {
		where += fmt.Sprintf(", valid until %s", expires.Local().Format("2 Jan 2006"))
	}
	fmt.Fprintln(env.Stdout, where+".")
	return nil
}

func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// browserLogin runs the native sign-in (RFC 8252): the browser goes to the server's
// /auth/login, the provider sends it back through the server to a loopback port this process
// listens on, and the one-time code that arrives there is traded, with the PKCE verifier only
// this process knows, for a token.
func browserLogin(ctx context.Context, env *Env, c *Client, launch bool) (tokenResponse, error) {
	ac, err := c.AuthConfig(ctx)
	if err != nil {
		return tokenResponse{}, err
	}
	if !ac.OIDC {
		return tokenResponse{}, coded(exitUserErr, "%s does not offer sign-in through a browser; make a token in the admin panel and run gator login --token -", c.Base)
	}
	// 127.0.0.1 rather than localhost: the name can resolve to ::1 first, where nothing listens.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return tokenResponse{}, fmt.Errorf("listening for the browser: %w", err)
	}
	defer ln.Close()
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)
	state := randomString()
	verifier := oauth2.GenerateVerifier()
	q := url.Values{
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"code_challenge":        {oauth2.S256ChallengeFromVerifier(verifier)},
		"code_challenge_method": {"S256"},
	}
	loginURL := c.Base + "/auth/login?" + q.Encode()

	type result struct {
		code string
		err  error
	}
	results := make(chan result, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/callback" {
			http.NotFound(w, r)
			return
		}
		got := r.URL.Query()
		// A request without our state is not the sign-in we started — another tab, or someone
		// probing the port. Turn it away and keep waiting for the real one.
		if subtle.ConstantTimeCompare([]byte(got.Get("state")), []byte(state)) != 1 {
			http.Error(w, "This sign-in was not started by this gator command.", http.StatusBadRequest)
			return
		}
		res := result{code: got.Get("code")}
		if e := got.Get("error"); e != "" {
			res = result{err: coded(exitAuth, "sign-in did not complete: %s", e)}
		} else if res.code == "" {
			res = result{err: coded(exitAuth, "the server sent the browser back without a code")}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if res.err != nil {
			fmt.Fprintf(w, page, "Sign-in did not complete", html.EscapeString(res.err.Error()))
		} else {
			fmt.Fprintf(w, page, "Signed in", "You can close this tab and return to the terminal.")
		}
		// The page must reach the browser before the listener goes away with this command.
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		select {
		case results <- res:
		default:
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		done, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(done)
	}()

	fmt.Fprintf(env.Stderr, "Sign in with %s in your browser. If it does not open, visit:\n\n  %s\n\n", orDefault(ac.Provider, "your identity provider"), loginURL)
	if launch && env.OpenBrowser != nil {
		_ = env.OpenBrowser(loginURL)
	}

	timeout := env.LoginTimeout
	if timeout == 0 {
		timeout = 5 * time.Minute
	}
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var res result
	select {
	case res = <-results:
	case <-wait.Done():
		if ctx.Err() != nil {
			return tokenResponse{}, ctx.Err()
		}
		return tokenResponse{}, coded(exitAuth, "gave up waiting for the browser after %s", timeout)
	}
	if res.err != nil {
		return tokenResponse{}, res.err
	}
	return c.ExchangeCode(ctx, tokenRequest{
		Code: res.code, CodeVerifier: verifier, RedirectURI: redirectURI,
		ClientName: "gator CLI on " + orDefault(env.Hostname, "this machine"),
	})
}

const page = `<!doctype html><meta charset="utf-8"><title>Gator</title>
<body style="font: 16px -apple-system, system-ui, sans-serif; margin: 15vh auto; max-width: 32em; text-align: center">
<h1 style="font-weight: 600">%s</h1><p>%s</p></body>`

func randomString() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func cmdLogout(ctx context.Context, env *Env, args []string) error {
	fs, g := newFlags("logout")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	cfg, err := LoadConfig(env.ConfigDir)
	if err != nil {
		return err
	}
	server, err := serverFor(env, g, cfg)
	if err != nil {
		return err
	}
	token, err := env.Store.Get(server)
	if errors.Is(err, ErrNoToken) {
		fmt.Fprintf(env.Stdout, "Not signed in to %s.\n", server)
		return nil
	}
	if err != nil {
		return err
	}
	// Revoke first: a token forgotten here but alive on the server is still a token.
	revokeErr := (&Client{Base: server, Token: token, HTTP: env.HTTP}).Logout(ctx)
	var ae *APIError
	if errors.As(revokeErr, &ae) && ae.Status == http.StatusUnauthorized {
		revokeErr = nil // already expired or revoked
	}
	if err := env.Store.Delete(server); err != nil && !errors.Is(err, ErrNoToken) {
		return err
	}
	if revokeErr != nil {
		return fmt.Errorf("forgot the token here, but %s did not revoke it (%v); revoke \"gator CLI on %s\" in Gator's list of tokens", server, revokeErr, orDefault(env.Hostname, "this machine"))
	}
	fmt.Fprintf(env.Stdout, "Signed out of %s.\n", server)
	return nil
}

func cmdWhoami(ctx context.Context, env *Env, args []string) error {
	fs, g := newFlags("whoami")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	s, err := open(env, g)
	if err != nil {
		return err
	}
	me, err := s.client.Me(ctx)
	if err != nil {
		return err
	}
	if g.json {
		return printJSON(env, map[string]any{"server": s.server, "me": me, "project": s.cfg.Project})
	}
	fmt.Fprintf(env.Stdout, "%s (%s, workspace %s) on %s\n", me.Name, me.Kind, me.WorkspaceRole, s.server)
	if s.cfg.Project != "" {
		fmt.Fprintf(env.Stdout, "Current project: %s\n", s.cfg.Project)
	}
	return nil
}
