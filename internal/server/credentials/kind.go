// Package credentials keeps people's own model logins — a Claude subscription, an Anthropic API
// key, a ChatGPT login for Codex, an OpenAI API key — and hands the one that pays for a job to the
// runner that leases it.
//
// A login is stored encrypted with the server's keyring (GATOR_SECRETS_KEY) and leaves the server
// only inside a lease, over the runner's authenticated connection. The API shows its kind, a hint
// (its last four characters) and how it stands, never its value.
//
// What a login is — how it is pasted, what a job's agent is given, whether its CLI rewrites it — is
// a Kind, registered in a Registry. Adding one is writing a Kind and registering it; nothing else
// in Gator switches on kinds.
package credentials

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/onegator/gator/internal/proto"
)

// Kind is one way of being logged in to a model.
type Kind interface {
	// Name is the kind in the API and the database: claude_subscription.
	Name() string
	// Backend is the runner backend this login runs: "claude", "acp:codex".
	Backend() string
	// Describe is what a person is told of the kind: what it is called and how to get one.
	Describe() Description
	// Parse checks what a person pasted, by its shape alone (no network), and returns it as it is
	// stored. Its error never quotes the value.
	Parse(raw string) (Secret, error)
	// Lease is how a job's agent gets the login. The service fills in the ID.
	Lease(secret string) proto.Credential
	// Sensitive are the values of the login that must never be shown: what logs, events and
	// transcripts are masked of.
	Sensitive(secret string) []string
}

// Description is what a person reads of a kind.
type Description struct {
	Label string // "Claude subscription"
	Help  string // how to get one
}

// Secret is a login as it is stored: its value, and what may be shown of it.
type Secret struct {
	Value string
	Hint  string
}

// Rotating is a kind whose CLI rewrites the login while it works: an OAuth login whose refresh
// token is replaced on every refresh. A job holds such a login alone — two jobs refreshing it
// would each make the other's token invalid — and what the job rewrote replaces the stored one.
type Rotating interface {
	Kind
	// Refreshed is the stored login after a job rewrote the files of its Home.Refresh.
	Refreshed(old string, files map[string][]byte) (Secret, error)
}

// Checker is a kind that can try a login with one small real call.
type Checker interface {
	Check(ctx context.Context, secret string) error
}

// Errors of the package.
var (
	ErrUnknownKind = errors.New("no such kind of login")
	ErrInvalid     = errors.New("invalid login")
	ErrNotFound    = errors.New("no such login")
	ErrNoKeyring   = errors.New("the server has no secrets key (GATOR_SECRETS_KEY), so it cannot keep logins")
	ErrCannotCheck = errors.New("this kind of login cannot be checked from the server")
	// ErrRefused is a login the provider turned down: wrong, expired or revoked.
	ErrRefused = errors.New("refused")
	// ErrUnavailable is a check that could not tell: the provider did not answer, or answered
	// something other than yes or no.
	ErrUnavailable = errors.New("could not check")
)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Registry is the kinds a server knows, in the order a backend prefers them.
type Registry struct {
	kinds []Kind
}

// NewRegistry registers kinds in order.
func NewRegistry(kinds ...Kind) *Registry {
	r := &Registry{}
	for _, k := range kinds {
		r.Register(k)
	}
	return r
}

// Register adds a kind after those already there: among the kinds of one backend, the first
// registered is the one a job uses when its payer has several. A name is registered once.
func (r *Registry) Register(k Kind) {
	if _, ok := r.Get(k.Name()); ok {
		panic("credentials: kind " + k.Name() + " registered twice")
	}
	r.kinds = append(r.kinds, k)
}

// Get is the kind of that name.
func (r *Registry) Get(name string) (Kind, bool) {
	i := slices.IndexFunc(r.kinds, func(k Kind) bool { return k.Name() == name })
	if i < 0 {
		return nil, false
	}
	return r.kinds[i], true
}

// All are the kinds, in registration order.
func (r *Registry) All() []Kind { return slices.Clone(r.kinds) }

// ForBackend are the kinds that run backend, the preferred first.
func (r *Registry) ForBackend(backend string) []Kind {
	var out []Kind
	for _, k := range r.kinds {
		if k.Backend() == backend {
			out = append(out, k)
		}
	}
	return out
}

// hint is what may be shown of a value: its last four characters.
func hint(v string) string {
	if len(v) <= 8 {
		return "…"
	}
	return "…" + v[len(v)-4:]
}
