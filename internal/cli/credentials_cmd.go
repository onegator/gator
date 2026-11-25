package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

// ModelCredential is one of the person's model logins, as the server shows it: never its value.
type ModelCredential struct {
	Kind       string     `json:"kind"`
	Backend    string     `json:"backend"`
	Label      string     `json:"label"`
	Hint       string     `json:"hint"`
	Status     string     `json:"status"`
	LastError  string     `json:"lastError"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	CheckedAt  *time.Time `json:"checkedAt,omitempty"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
}

// ModelCredentialKind is a kind of login the server knows.
type ModelCredentialKind struct {
	Kind       string `json:"kind"`
	Backend    string `json:"backend"`
	Label      string `json:"label"`
	Help       string `json:"help"`
	Preference int    `json:"preference"`
	Rotates    bool   `json:"rotates"`
	Checkable  bool   `json:"checkable"`
	Multiline  bool   `json:"multiline,omitempty"`
}

// ModelCredentials is GET /me/credentials.
type ModelCredentials struct {
	Credentials  []ModelCredential     `json:"credentials"`
	Kinds        []ModelCredentialKind `json:"kinds"`
	RunnerLogins bool                  `json:"runnerLogins"`
}

func (c *Client) Credentials(ctx context.Context) (ModelCredentials, error) {
	var out ModelCredentials
	return out, c.do(ctx, http.MethodGet, "/api/v1/me/credentials", nil, nil, &out)
}

func (c *Client) PutCredential(ctx context.Context, kind, secret string) (ModelCredential, error) {
	var out ModelCredential
	return out, c.do(ctx, http.MethodPut, "/api/v1/me/credentials/"+url.PathEscape(kind), nil, map[string]string{"secret": secret}, &out)
}

func (c *Client) CheckCredential(ctx context.Context, kind string) (ModelCredential, error) {
	var out ModelCredential
	return out, c.do(ctx, http.MethodPost, "/api/v1/me/credentials/"+url.PathEscape(kind)+"/check", nil, nil, &out)
}

func (c *Client) DeleteCredential(ctx context.Context, kind string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/me/credentials/"+url.PathEscape(kind), nil, nil, nil)
}

func cmdCredentials(ctx context.Context, env *Env, args []string) error {
	fs, g := newFlags("credentials")
	if _, err := parse(fs, args); err != nil {
		return err
	}
	s, err := open(env, g)
	if err != nil {
		return err
	}
	all, err := s.client.Credentials(ctx)
	if err != nil {
		return err
	}
	if g.json {
		return printJSON(env, all)
	}
	have := map[string]ModelCredential{}
	for _, c := range all.Credentials {
		have[c.Kind] = c
	}
	tw := table(env)
	fmt.Fprintln(tw, "KIND\tFOR\tLOGIN\tSTATUS")
	for _, k := range all.Kinds {
		c, ok := have[k.Kind]
		login, status := "-", "not connected"
		if ok {
			login, status = c.Hint, c.Status
			if c.LastError != "" {
				status += ": " + c.LastError
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", k.Kind, k.Backend, login, status)
	}
	_ = tw.Flush()
	if all.RunnerLogins {
		fmt.Fprintln(env.Stdout, "Jobs whose payer has no login of their own run on the runners' own logins in this workspace.")
	}
	return nil
}

// cmdCredentialSet reads the secret from stdin or a file, never from the command line, where a
// shell's history would keep it.
func cmdCredentialSet(ctx context.Context, env *Env, args []string) error {
	fs, g := newFlags("credentials set")
	file := fs.String("file", "", "read the login from this file (~/.codex/auth.json); default stdin")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	kind, err := oneArg(pos, "kind of login (gator credentials lists them)")
	if err != nil {
		return err
	}
	s, err := open(env, g)
	if err != nil {
		return err
	}
	var raw []byte
	switch f, isFile := env.Stdin.(*os.File); {
	case *file != "" && *file != "-":
		raw, err = os.ReadFile(*file)
	case isFile && term.IsTerminal(int(f.Fd())) && multiline(ctx, s.client, kind):
		return coded(exitUserErr, "a %s is a file's contents: pass --file (for Codex, --file ~/.codex/auth.json)", kind)
	case isFile && term.IsTerminal(int(f.Fd())):
		// Typed or pasted at a terminal: not echoed, so it is neither on the screen nor in a
		// terminal's scrollback.
		fmt.Fprintf(env.Stderr, "Paste the %s (it is not shown) and press Enter: ", kind)
		raw, err = term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(env.Stderr)
	default:
		raw, err = io.ReadAll(io.LimitReader(env.Stdin, 1<<20))
	}
	if err != nil {
		return coded(exitUserErr, "reading the login: %v", err)
	}
	secret := strings.TrimSpace(string(raw))
	if secret == "" {
		return coded(exitUserErr, "no login given: pipe it on stdin or pass --file")
	}
	c, err := s.client.PutCredential(ctx, kind, secret)
	if err != nil {
		return err
	}
	if g.json {
		return printJSON(env, c)
	}
	fmt.Fprintf(env.Stdout, "Saved your %s (%s). Jobs you pay for run on it; gator credentials check %s tries it.\n", c.Label, c.Hint, kind)
	return nil
}

func cmdCredentialCheck(ctx context.Context, env *Env, args []string) error {
	fs, g := newFlags("credentials check")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	kind, err := oneArg(pos, "kind of login")
	if err != nil {
		return err
	}
	s, err := open(env, g)
	if err != nil {
		return err
	}
	c, err := s.client.CheckCredential(ctx, kind)
	if err != nil {
		return err
	}
	if g.json {
		return printJSON(env, c)
	}
	if c.Status == "error" {
		return coded(exitUserErr, "your %s was turned down: %s", c.Label, c.LastError)
	}
	fmt.Fprintf(env.Stdout, "Your %s (%s) works.\n", c.Label, c.Hint)
	return nil
}

func cmdCredentialRemove(ctx context.Context, env *Env, args []string) error {
	fs, g := newFlags("credentials remove")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	kind, err := oneArg(pos, "kind of login")
	if err != nil {
		return err
	}
	s, err := open(env, g)
	if err != nil {
		return err
	}
	if err := s.client.DeleteCredential(ctx, kind); err != nil {
		return err
	}
	if !g.json {
		fmt.Fprintf(env.Stdout, "Removed your %s.\n", kind)
	}
	return nil
}

// multiline says whether the server keeps a login of kind as a file's contents, which a terminal's
// hidden prompt reads only the first line of.
func multiline(ctx context.Context, c *Client, kind string) bool {
	all, err := c.Credentials(ctx)
	if err != nil {
		return false
	}
	for _, k := range all.Kinds {
		if k.Kind == kind {
			return k.Multiline
		}
	}
	return false
}
