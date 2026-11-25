// Package bridges is the core's side of messaging plugins: Slack, a code host's comments, later
// Teams and Telegram. A bridge connects outside places (a Slack channel, a pull request) to a
// project's conversations, posts what people said there, and mirrors what is said here back out.
// The core knows no outside system; it keeps the rules every bridge is held to:
//
//   - everything is the bridge's own project, and only conversations it connected itself;
//   - an outside account is an identity of that project and that bridge alone; it becomes a person
//     only with a code that person made in Gator, never by an email address;
//   - what an outside identity says is untrusted: agents hear it in chat, as data, and it approves
//     nothing and allows no agent to work;
//   - nothing private leaves: a private channel or a DM is never connected or mirrored;
//   - a bridge never hears back what it brought in (no echo).
package bridges

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/limits"
	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/plugin"
)

// Bridge is who calls: one plugin in one project. The plugins host says so, from the process the
// call came from; nothing in a call's parameters can change it.
type Bridge struct {
	Project pgtype.UUID
	// Provider is the plugin's name, the namespace of everything it brings in.
	Provider string
}

// threads finds the thread under a message and a task's thread. conversations.Threads is one.
type threads interface {
	Under(ctx context.Context, msg db.Message) (db.Conversation, error)
	ForTask(ctx context.Context, task db.Task) (db.Conversation, error)
}

// writer writes what a bridge carries in. conversations.Bridged is one.
type writer interface {
	Post(ctx context.Context, p conversations.BridgedPost) (db.Message, bool, error)
	Edit(ctx context.Context, project pgtype.UUID, provider, externalRef, body string) (db.Message, error)
	BridgeChannel(ctx context.Context, project pgtype.UUID, name, topic string) (db.Conversation, bool, error)
}

// rights answers what a person may do in a conversation. conversations.Access is one.
type rights interface {
	UserCanWrite(ctx context.Context, conv db.Conversation, user pgtype.UUID) (bool, error)
	OpenToProject(ctx context.Context, conv db.Conversation) (bool, error)
}

// roles answers a person's role in a project. auth.Authorizer is one.
type roles interface {
	ProjectRole(ctx context.Context, p auth.Principal, project pgtype.UUID) (auth.Role, error)
}

// approver approves what a task's gate waits for, as a person. process.Service is one.
type approver interface {
	Approve(ctx context.Context, taskID pgtype.UUID, userID pgtype.UUID) error
}

// Deps are what the package needs from the rest of the server.
type Deps struct {
	Threads  threads
	Writer   writer
	Rights   rights
	Roles    roles
	Approver approver
	// Now is the clock of link codes and of how often an edit goes out; nil is time.Now.
	Now func() time.Time
}

// Service serves the messaging core methods and mirrors conversations out.
type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
	deps Deps
	// links bounds how often one outside account may try a code: a code is short enough to type.
	links *limits.Limiter

	mu   sync.Mutex
	sent map[editKey]time.Time // when an edit of a message last went out to a bridge
}

// New builds the service.
func New(pool *pgxpool.Pool, deps Deps) *Service {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	return &Service{pool: pool, q: db.New(pool), deps: deps, links: limits.NewLimiter(5.0/60, 5),
		sent: map[editKey]time.Time{}}
}

func invalid(format string, a ...any) error {
	return plugin.Errorf(plugin.CodeInvalidParams, format, a...)
}

func notFound(format string, a ...any) error {
	return plugin.Errorf(plugin.CodeNotFound, format, a...)
}

func forbidden(format string, a ...any) error {
	return plugin.Errorf(plugin.CodeForbidden, format, a...)
}

func parseUUID(s string) (pgtype.UUID, bool) {
	var u pgtype.UUID
	return u, u.Scan(s) == nil && u.Valid
}

// Limits of what a bridge names.
const (
	maxRef         = 400
	maxExternalID  = 200
	maxDisplayName = 80
)

func validRef(s string) bool {
	return s != "" && len(s) <= maxRef && utf8.ValidString(s) && !strings.ContainsFunc(s, unicode.IsControl)
}

// displayName keeps of an outside name one line of at most 80 characters.
func displayName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxDisplayName {
		s = string(r[:maxDisplayName])
	}
	return s
}
