package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/runner/client"
)

// login is the model login of the person who pays for a job, as its agent gets it: variables in
// its environment and, for a CLI that keeps its login in files, a directory of the job's own.
// Nothing of it is written anywhere else, and the directory goes when the job ends.
type login struct {
	cred *proto.Credential
	env  []string
	home string
}

// openLogin prepares c for one job; nil for a job on the runner's own login.
func openLogin(c *proto.Credential) (*login, error) {
	if c == nil {
		return nil, nil
	}
	l := &login{cred: c}
	names := make([]string, 0, len(c.Env))
	for k := range c.Env {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		if !loginVariable(k) {
			return nil, fmt.Errorf("the job's login sets %q, which is not a login's variable", k)
		}
		l.env = append(l.env, k+"="+c.Env[k])
	}
	if h := c.Home; h != nil {
		if !loginVariable(h.Env) {
			return nil, fmt.Errorf("the job's login names its directory in %q, which is not a login's variable", h.Env)
		}
		dir, err := os.MkdirTemp("", "gator-login-") // 0700
		if err != nil {
			return nil, err
		}
		l.home = dir
		for name, data := range h.Files {
			if name != filepath.Base(name) || name == "." || name == ".." {
				l.close()
				return nil, fmt.Errorf("the job's login has a file %q outside its directory", name)
			}
			if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
				l.close()
				return nil, err
			}
		}
		l.env = append(l.env, h.Env+"="+dir)
	}
	return l, nil
}

var variableName = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// loginVariable says whether a server may set k for a login: a variable's name, and not one of
// those that run the runner itself or decide what a process loads.
func loginVariable(k string) bool {
	if !variableName.MatchString(k) {
		return false
	}
	for _, p := range []string{"GATOR_", "LD_", "DYLD_"} {
		if strings.HasPrefix(k, p) {
			return false
		}
	}
	return !slices.Contains([]string{"PATH", "HOME", "SHELL", "USER", "TMPDIR", "NODE_OPTIONS"}, k)
}

// Env is what the agent's environment gets of the login.
func (l *login) Env() []string {
	if l == nil {
		return nil
	}
	return l.env
}

// refreshed is what the agent's CLI rewrote of the login's files, to send back; nil when nothing.
func (l *login) refreshed() *proto.CredentialRefresh {
	if l == nil || l.home == "" || l.cred.Home == nil {
		return nil
	}
	files := map[string][]byte{}
	for _, name := range l.cred.Home.Refresh {
		if name != filepath.Base(name) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(l.home, name))
		if err != nil || len(b) == 0 || bytes.Equal(b, l.cred.Home.Files[name]) {
			continue
		}
		files[name] = b
	}
	if len(files) == 0 {
		return nil
	}
	return &proto.CredentialRefresh{ID: l.cred.ID, Files: files}
}

func (l *login) close() {
	if l != nil && l.home != "" {
		_ = os.RemoveAll(l.home)
	}
}

// mask replaces every value of the login in s.
func (l *login) mask(s string) string {
	if l == nil {
		return s
	}
	for _, v := range l.cred.Redact {
		if len(v) >= 8 {
			s = strings.ReplaceAll(s, v, "[redacted]")
		}
	}
	return s
}

// maskIO masks the login's values in everything the job sends while it runs: its events, and what
// it says when it asks to end its turn.
func (l *login) maskIO(io client.JobIO) client.JobIO {
	if l == nil || len(l.cred.Redact) == 0 {
		return io
	}
	emit := io.Emit
	io.Emit = func(typ string, payload any) {
		b, err := json.Marshal(payload)
		if err != nil {
			emit(typ, payload)
			return
		}
		emit(typ, json.RawMessage(l.mask(string(b))))
	}
	if ask := io.TurnEnding; ask != nil {
		io.TurnEnding = func(te proto.TurnEnding) []proto.Objection {
			te.Summary = l.mask(te.Summary)
			return ask(te)
		}
	}
	return io
}

// maskFinish masks the login's values in the receipt.
func (l *login) maskFinish(fin *proto.Finish) {
	if l == nil {
		return
	}
	fin.Summary, fin.StopReason = l.mask(fin.Summary), l.mask(fin.StopReason)
	if d := fin.Digest; d != nil {
		for _, list := range [][]string{d.Changes, d.Decisions, d.Rejected, d.Left} {
			for i := range list {
				list[i] = l.mask(list[i])
			}
		}
	}
}

// refusals are how the backends say a login was turned down: an invalid or revoked key, an
// expired OAuth token, a ChatGPT login whose refresh token was used elsewhere.
var refusals = regexp.MustCompile(`(?i)invalid (x-)?api[ _-]?key|authentication_error|invalid bearer token|` +
	`oauth token (has )?(expired|been revoked)|token (has )?(expired|been revoked)|please run /login|not logged in|` +
	`refresh_token_reused|refresh token (was|has been) (already )?used|could not be refreshed|\b401\b[^\n]{0,40}(unauthori[sz]ed|authentication)`)

// refused is, for a failed job on its payer's login, the backend's words that the login was turned
// down, masked; empty when the job failed for another reason.
func (l *login) refused(fin proto.Finish) string {
	if l == nil || fin.Status != proto.StatusFailed {
		return ""
	}
	for _, said := range []string{fin.StopReason, fin.Summary} {
		if m := refusals.FindStringIndex(said); m != nil {
			return clipText(l.mask(strings.TrimSpace(said)), 300)
		}
	}
	return ""
}

func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
