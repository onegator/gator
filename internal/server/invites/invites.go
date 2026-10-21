// Package invites lets a project admin bring in people by email, before they have an account.
//
// An invitation is a token for one address and one project. The person accepts it either by
// signing in with that address, which takes every open invitation for it at once, or by
// opening the link while signed in. Only the token's hash is stored; the link is shown to
// the inviter once, and renewing an invitation is how they get another.
package invites

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

// TTL is how long a link stays good. Long enough for someone back from a holiday, short
// enough that a link lost in a chat history does not open the project forever.
const TTL = 14 * 24 * time.Hour

var (
	// ErrUnknown covers a token that was never issued, was revoked, replaced, expired or
	// already taken by someone else: none of them says anything useful to whoever holds it.
	ErrUnknown = errors.New("the invitation is unknown, revoked or expired")
	// ErrWrongEmail means the invitation was sent to another address than the signed-in one.
	ErrWrongEmail = errors.New("this invitation was sent to another email address")
	// ErrInvalidEmail is an address nobody could sign in with.
	ErrInvalidEmail = errors.New("not an email address")
)

// NormalizeEmail trims and lower-cases an address, refusing anything with a display name or
// without a domain. Matching is case-insensitive anyway; storing one form keeps the list tidy.
func NormalizeEmail(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || !strings.Contains(s[strings.LastIndex(s, "@")+1:], ".") {
		return "", ErrInvalidEmail
	}
	return s, nil
}

// NewToken returns a fresh token and the hash that is stored for it.
func NewToken() (token string, hash []byte) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, Hash(token)
}

// Hash is what the database knows of a token.
func Hash(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// AppLink opens Gator.app on the invitation.
func AppLink(token string) string { return "gator://invite/" + token }

// WebLink is the same invitation as an address any mail or chat program makes clickable;
// the server's page there hands it on to the app.
func WebLink(base, token string) string {
	return strings.TrimRight(base, "/") + "/invite/" + token
}

// Message is what a Mailer is asked to deliver.
type Message struct {
	To          string
	ProjectName string
	InviterName string
	Role        string
	Link        string
	AppLink     string
	ExpiresAt   time.Time
}

// Mailer delivers an invitation. Sent reports whether the person will hear from the server;
// when it is false the inviter is told to pass the link on themselves.
type Mailer interface {
	SendInvitation(ctx context.Context, m Message) (sent bool, err error)
}

// LogMailer is the mailer of a server that has no mail set up: it notes that an invitation
// was made and leaves the delivery to the inviter. It never logs the link, which is as good
// as the invitation itself.
type LogMailer struct{ Log *slog.Logger }

func (l LogMailer) SendInvitation(_ context.Context, m Message) (bool, error) {
	if l.Log != nil {
		l.Log.Info("invitation not emailed: no mailer configured, the inviter passes the link on",
			"to", m.To, "project", m.ProjectName)
	}
	return false, nil
}

// Waiting reports whether email has an invitation it could still accept. It lets a person
// create an account that the allowed domains would otherwise refuse: an admin of a project
// vouched for them.
func Waiting(ctx context.Context, q *db.Queries, email string) (bool, error) {
	return q.HasOpenInvitationForEmail(ctx, email)
}

// AcceptAllFor takes every open invitation sent to the user's email, as happens when they
// sign in. It returns the invitations it took.
func AcceptAllFor(ctx context.Context, pool *pgxpool.Pool, user db.User) ([]db.ProjectInvitation, error) {
	var taken []db.ProjectInvitation
	err := store.InTx(ctx, pool, func(q *db.Queries) error {
		var err error
		taken, err = q.TakeInvitationsForEmail(ctx, db.TakeInvitationsForEmailParams{UserID: user.ID, Email: user.Email})
		if err != nil {
			return err
		}
		for _, inv := range taken {
			if err := join(ctx, q, inv, user.ID); err != nil {
				return err
			}
		}
		return nil
	})
	return taken, err
}

// Accept takes the invitation behind token for the signed-in user, whose email must be the
// one invited. Accepting an invitation one already took answers it again, so a link opened
// twice does not look like a failure.
func Accept(ctx context.Context, pool *pgxpool.Pool, token string, user db.User) (db.ProjectInvitation, error) {
	var out db.ProjectInvitation
	err := store.InTx(ctx, pool, func(q *db.Queries) error {
		inv, err := q.GetInvitationByTokenHash(ctx, Hash(token))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnknown
		}
		if err != nil {
			return err
		}
		if !strings.EqualFold(inv.Email, user.Email) {
			return ErrWrongEmail
		}
		if inv.AcceptedAt.Valid && inv.AcceptedBy.Valid && inv.AcceptedBy.Bytes == user.ID.Bytes {
			out = inv
			return nil
		}
		inv, err = q.TakeInvitation(ctx, db.TakeInvitationParams{ID: inv.ID, UserID: user.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnknown
		}
		if err != nil {
			return err
		}
		out = inv
		return join(ctx, q, inv, user.ID)
	})
	return out, err
}

func join(ctx context.Context, q *db.Queries, inv db.ProjectInvitation, user pgtype.UUID) error {
	if err := q.JoinProject(ctx, db.JoinProjectParams{ProjectID: inv.ProjectID, UserID: user, Role: inv.Role}); err != nil {
		return err
	}
	return Emit(ctx, q, "project.invitation_accepted", inv.ProjectID, map[string]any{
		"invitationId": uuidString(inv.ID), "userId": uuidString(user), "role": inv.Role,
	})
}

// Emit records a membership event on the project, for the relay to publish.
func Emit(ctx context.Context, q *db.Queries, typ string, project pgtype.UUID, payload map[string]any) error {
	b, _ := json.Marshal(payload)
	_, err := q.InsertEvent(ctx, db.InsertEventParams{Type: typ, Aggregate: "project", AggregateID: project, Payload: b})
	return err
}

func uuidString(u pgtype.UUID) string {
	v, _ := u.Value()
	s, _ := v.(string)
	return s
}
