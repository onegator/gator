package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/redact"
	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/secrets"
	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

// Statuses of a login.
const (
	StatusUnchecked = "unchecked"
	StatusOK        = "ok"
	StatusError     = "error"
)

// View is a login as the API shows it: never its value.
type View struct {
	Kind       string
	Backend    string
	Label      string
	Hint       string
	Status     string
	LastError  string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	CheckedAt  *time.Time
	LastUsedAt *time.Time
}

// Service keeps people's logins and decides which one a job runs on (runners.Credentials).
type Service struct {
	pool  *pgxpool.Pool
	keys  *secrets.Keyring
	kinds *Registry
	log   *slog.Logger
	// changed is told when a login that jobs may wait for comes or goes: the runner manager offers
	// queued work again.
	changed func()
}

// Deps are what the service is built from.
type Deps struct {
	Keys  *secrets.Keyring
	Kinds *Registry // default DefaultKinds(Endpoints{})
	Log   *slog.Logger
	// Changed is called after a login is saved, removed or the workspace's rule changes; optional.
	Changed func()
}

// New builds the service.
func New(pool *pgxpool.Pool, d Deps) *Service {
	if d.Kinds == nil {
		d.Kinds = DefaultKinds(Endpoints{})
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Changed == nil {
		d.Changed = func() {}
	}
	return &Service{pool: pool, keys: d.Keys, kinds: d.Kinds, log: d.Log, changed: d.Changed}
}

// OnChange has fn told when a login that jobs may wait for comes or goes. It is wiring, for a
// runner manager built after the service.
func (s *Service) OnChange(fn func()) { s.changed = fn }

// Kinds are the kinds this server knows.
func (s *Service) Kinds() []Kind { return s.kinds.All() }

// List is a person's logins.
func (s *Service) List(ctx context.Context, user pgtype.UUID) ([]View, error) {
	rows, err := db.New(s.pool).ListUserCredentials(ctx, user)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(rows))
	for _, r := range rows {
		out = append(out, s.view(r))
	}
	return out, nil
}

// Put saves a person's login of a kind, replacing the one they had. It checks the shape only.
func (s *Service) Put(ctx context.Context, user pgtype.UUID, kind, raw string) (View, error) {
	k, ok := s.kinds.Get(kind)
	if !ok {
		return View{}, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	sec, err := k.Parse(raw)
	if err != nil {
		return View{}, err
	}
	sealed, err := s.seal(sec.Value)
	if err != nil {
		return View{}, err
	}
	var row db.UserCredential
	err = s.locked(ctx, func(q *db.Queries) error {
		var err error
		if row, err = q.UpsertUserCredential(ctx, db.UpsertUserCredentialParams{UserID: user, Kind: kind, Backend: k.Backend(),
			Secret: sealed, Hint: sec.Hint}); err != nil {
			return err
		}
		if _, err := q.ReleaseWaitingJobs(ctx, user); err != nil {
			return err
		}
		return emit(ctx, q, "credential.saved", user, map[string]any{"kind": kind})
	})
	if err != nil {
		return View{}, err
	}
	s.changed()
	return s.view(row), nil
}

// Delete removes a person's login of a kind. A job leased with it keeps running; the next waits.
func (s *Service) Delete(ctx context.Context, user pgtype.UUID, kind string) error {
	if _, ok := s.kinds.Get(kind); !ok {
		return fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	err := s.locked(ctx, func(q *db.Queries) error {
		n, err := q.DeleteUserCredential(ctx, db.DeleteUserCredentialParams{UserID: user, Kind: kind})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		if _, err := q.ReleaseWaitingJobs(ctx, user); err != nil {
			return err
		}
		return emit(ctx, q, "credential.removed", user, map[string]any{"kind": kind})
	})
	if err == nil {
		s.changed()
	}
	return err
}

// Check tries a person's login with one small call to its provider. A refusal marks it; a check
// that could not tell leaves it as it was and returns ErrUnavailable.
func (s *Service) Check(ctx context.Context, user pgtype.UUID, kind string) (View, error) {
	k, ok := s.kinds.Get(kind)
	if !ok {
		return View{}, fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
	checker, ok := k.(Checker)
	if !ok {
		return View{}, ErrCannotCheck
	}
	q := db.New(s.pool)
	row, err := q.GetUserCredential(ctx, db.GetUserCredentialParams{UserID: user, Kind: kind})
	if errors.Is(err, pgx.ErrNoRows) {
		return View{}, ErrNotFound
	}
	if err != nil {
		return View{}, err
	}
	secret, err := s.open(row)
	if err != nil {
		return View{}, err
	}
	err = checker.Check(ctx, secret)
	switch {
	case errors.Is(err, ErrUnavailable), errors.Is(err, ErrCannotCheck):
		return s.view(row), err
	case errors.Is(err, ErrUncertain):
		row, err = s.setStatus(ctx, q, row, StatusUnchecked, redact.Values(err.Error(), k.Sensitive(secret)), true)
	case err != nil:
		row, err = s.setStatus(ctx, q, row, StatusError, redact.Values(err.Error(), k.Sensitive(secret)), true)
	default:
		row, err = s.setStatus(ctx, q, row, StatusOK, "", true)
	}
	if err != nil {
		return View{}, err
	}
	s.changed()
	return s.view(row), nil
}

// RunnerLogins says whether a job whose payer has no login of their own runs on the runner's.
func (s *Service) RunnerLogins(ctx context.Context) (bool, error) {
	ws, err := db.New(s.pool).GetWorkspaceSettings(ctx)
	return ws.RunnerLogins, err
}

// SetRunnerLogins sets that rule for the workspace.
func (s *Service) SetRunnerLogins(ctx context.Context, on bool) error {
	err := s.locked(ctx, func(q *db.Queries) error {
		if _, err := q.GetWorkspaceSettings(ctx); err != nil { // the row, if a test emptied the table
			return err
		}
		if _, err := q.SetRunnerLogins(ctx, on); err != nil {
			return err
		}
		_, err := q.ReleaseAllWaitingJobs(ctx)
		return err
	})
	if err == nil {
		s.changed()
	}
	return err
}

func (s *Service) setStatus(ctx context.Context, q *db.Queries, row db.UserCredential, status, lastErr string, checked bool) (db.UserCredential, error) {
	p := db.SetUserCredentialStatusParams{ID: row.ID, Status: status, LastError: lastErr}
	if checked {
		p.CheckedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	}
	return q.SetUserCredentialStatus(ctx, p)
}

func (s *Service) seal(v string) (string, error) {
	if s.keys == nil {
		return "", ErrNoKeyring
	}
	return s.keys.Encrypt([]byte(v))
}

func (s *Service) open(row db.UserCredential) (string, error) {
	if s.keys == nil {
		return "", ErrNoKeyring
	}
	b, err := s.keys.Decrypt(row.Secret)
	if err != nil {
		return "", fmt.Errorf("the stored %s cannot be read with this server's keys; save it again", row.Kind)
	}
	return string(b), nil
}

func (s *Service) view(r db.UserCredential) View {
	v := View{Kind: r.Kind, Backend: r.Backend, Hint: r.Hint, Status: r.Status, LastError: r.LastError,
		CreatedAt: r.CreatedAt.Time, UpdatedAt: r.UpdatedAt.Time}
	if k, ok := s.kinds.Get(r.Kind); ok {
		v.Label = k.Describe().Label
	}
	if r.CheckedAt.Valid {
		t := r.CheckedAt.Time
		v.CheckedAt = &t
	}
	if r.LastUsedAt.Valid {
		t := r.LastUsedAt.Time
		v.LastUsedAt = &t
	}
	return v
}

func (s *Service) tx(ctx context.Context, fn func(q *db.Queries) error) error {
	return store.InTx(ctx, s.pool, fn)
}

// locked runs fn in a transaction that holds the lease lock first. A lease takes the lock, then
// the queued jobs, then their payers' logins; whatever changes a login and the jobs waiting for it
// (saving, removing, refusing it, the workspace's rule) takes them in that same order, so the two
// never wait on each other.
func (s *Service) locked(ctx context.Context, fn func(q *db.Queries) error) error {
	return s.tx(ctx, func(q *db.Queries) error {
		if err := q.LockLeases(ctx); err != nil {
			return err
		}
		return fn(q)
	})
}

// emit records an event about a person's logins. It names the kind, never a value.
func emit(ctx context.Context, q *db.Queries, typ string, user pgtype.UUID, payload map[string]any) error {
	b, _ := json.Marshal(payload)
	_, err := q.InsertEvent(ctx, db.InsertEventParams{Type: typ, Aggregate: events.AggregateUser, AggregateID: user, Payload: b})
	return err
}
