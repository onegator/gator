package credentials

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/runners"
	"github.com/onegator/gator/internal/server/store/db"
)

var _ runners.Credentials = (*Service)(nil)

// Where a person connects their logins, for the reasons a job waits.
const settingsPlace = "Settings → Model logins"

// Choose implements runners.Credentials. A job runs on its payer's login of a kind that runs its
// backend, the backend's preferred kind first, skipping one that was refused and one another job
// holds. Without any such login it runs on the runner's own when the workspace allows that, and
// waits when it does not. A payer whose logins are refused or busy waits regardless: they have
// their own, and somebody else's is not used in its place. The backend is never changed.
func (s *Service) Choose(ctx context.Context, q *db.Queries, job db.Job, open bool) (runners.CredentialChoice, error) {
	kinds := s.kinds.ForBackend(job.Backend)
	if !job.PayerID.Valid {
		return s.runnerOr(ctx, q, "Nobody pays for this job: no person asked for it, created its task or switched on what did. "+
			"A workspace admin can let jobs like it run on the runners' own logins.")
	}
	who := s.name(ctx, q, job.PayerID)
	if len(kinds) == 0 {
		return s.runnerOr(ctx, q, fmt.Sprintf("Gator cannot run %s on a person's own login, and this workspace does not lend jobs "+
			"the runners' logins. A workspace admin can allow that, or the project can use another backend.", job.Backend))
	}
	rows, err := q.ListUserCredentials(ctx, job.PayerID)
	if err != nil {
		return runners.CredentialChoice{}, err
	}
	byKind := map[string]db.UserCredential{}
	for _, r := range rows {
		byKind[r.Kind] = r
	}
	var refused, busy []string
	for _, k := range kinds {
		row, ok := byKind[k.Name()]
		if !ok {
			continue
		}
		label := k.Describe().Label
		if row.Status == StatusError {
			refused = append(refused, fmt.Sprintf("%s (%s)", label, strings.TrimSuffix(row.LastError, ".")))
			continue
		}
		_, rotating := k.(Rotating)
		if rotating {
			held, err := q.CredentialHeld(ctx, db.CredentialHeldParams{CredentialID: row.ID, JobID: job.ID})
			if err != nil {
				return runners.CredentialChoice{}, err
			}
			if held {
				busy = append(busy, label)
				continue
			}
		}
		choice := runners.CredentialChoice{Kind: k.Name(), ID: row.ID, Exclusive: rotating, Version: row.Version}
		if open {
			secret, err := s.open(row)
			if err != nil {
				if _, err := s.setStatus(ctx, q, row, StatusError, err.Error(), false); err != nil {
					return runners.CredentialChoice{}, err
				}
				refused = append(refused, fmt.Sprintf("%s (%s)", label, err))
				continue
			}
			c := k.Lease(secret)
			c.ID = uuidString(row.ID)
			choice.Credential = &c
			if err := q.TouchUserCredential(ctx, row.ID); err != nil {
				return runners.CredentialChoice{}, err
			}
		}
		return choice, nil
	}
	switch {
	case len(busy) > 0:
		return wait(fmt.Sprintf("%s's %s is in use by another job, and one job at a time may use it; this one starts when that one ends.",
			who, strings.Join(busy, " and "))), nil
	case len(refused) > 0:
		return wait(fmt.Sprintf("%s's %s stopped working. %s can connect it again in %s.",
			who, strings.Join(refused, " and "), who, settingsPlace)), nil
	}
	return s.runnerOr(ctx, q, fmt.Sprintf("Waiting for %s to connect %s in %s.", who, anyOf(kinds), settingsPlace))
}

// runnerOr is the runner's own login when the workspace lends it, else a wait for the reason.
func (s *Service) runnerOr(ctx context.Context, q *db.Queries, reason string) (runners.CredentialChoice, error) {
	on, err := q.RunnerLoginsAllowed(ctx)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return runners.CredentialChoice{}, err
	}
	if on {
		return runners.CredentialChoice{Kind: runners.RunnerLogin}, nil
	}
	return wait(reason), nil
}

func wait(reason string) runners.CredentialChoice { return runners.CredentialChoice{Wait: reason} }

// anyOf names the kinds a person could connect: "a Claude subscription or an Anthropic API key".
func anyOf(kinds []Kind) string {
	parts := make([]string, 0, len(kinds))
	for _, k := range kinds {
		label := k.Describe().Label
		article := "a "
		if strings.ContainsRune("AEIOU", rune(label[0])) {
			article = "an "
		}
		parts = append(parts, article+label)
	}
	return strings.Join(parts, " or ")
}

// name is how a waiting job names its payer to whoever reads the job: their display name, never
// their email address.
func (s *Service) name(ctx context.Context, q *db.Queries, user pgtype.UUID) string {
	if u, err := q.GetUser(ctx, user); err == nil && strings.TrimSpace(u.Name) != "" {
		return strings.TrimSpace(u.Name)
	}
	return "The person who pays for this job"
}

// Refreshed implements runners.Credentials: what the agent's CLI left of a rotating login replaces
// the stored one, because the one Gator had may have stopped working the moment it was refreshed.
func (s *Service) Refreshed(ctx context.Context, job db.Job, r proto.CredentialRefresh) error {
	if !job.CredentialID.Valid || r.ID != uuidString(job.CredentialID) {
		s.log.Warn("a runner sent back a login its job was not leased", "job", uuidString(job.ID))
		return nil // resending it would not make it this job's
	}
	q := db.New(s.pool)
	row, err := q.GetUserCredentialByID(ctx, job.CredentialID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // removed while the job ran: nothing to keep it for
	}
	if err != nil {
		return err
	}
	if row.Version != job.CredentialVersion {
		s.log.Info("a job's refreshed login was not kept: its owner saved another since", "job", uuidString(job.ID))
		return nil
	}
	k, _ := s.kinds.Get(row.Kind)
	rot, ok := k.(Rotating)
	if !ok {
		s.log.Warn("a runner sent back a login of a kind jobs do not rewrite", "job", uuidString(job.ID), "kind", row.Kind)
		return nil
	}
	old, err := s.open(row)
	if err != nil {
		_, serr := s.setStatus(ctx, q, row, StatusError, err.Error(), false)
		return serr
	}
	sec, err := rot.Refreshed(old, r.Files)
	if err != nil {
		_, serr := s.setStatus(ctx, q, row, StatusError, "what the agent left of this login after a job does not read: "+err.Error(), false)
		return serr
	}
	if sec.Value == old {
		return nil
	}
	sealed, err := s.seal(row.UserID, row.Kind, sec.Value)
	if err != nil {
		return err
	}
	n, err := q.ReplaceUserCredentialSecret(ctx, db.ReplaceUserCredentialSecretParams{ID: row.ID, Secret: sealed, Hint: sec.Hint,
		LeasedVersion: job.CredentialVersion})
	if err == nil && n == 0 {
		s.log.Info("a job's refreshed login was not kept: its owner saved another since", "job", uuidString(job.ID))
	}
	return err
}

// Refused implements runners.Credentials: the login is marked so that no job is leased with it
// again until its owner connects it anew, and its owner is told. A kind that can be checked is
// checked first, and a login its provider still takes is left alone: a backend's words are a
// reason to ask, not proof. It says whether the login was marked.
func (s *Service) Refused(ctx context.Context, job db.Job, reason string) (bool, error) {
	if !job.CredentialID.Valid {
		return false, nil
	}
	row, err := db.New(s.pool).GetUserCredentialByID(ctx, job.CredentialID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if k, ok := s.kinds.Get(row.Kind); ok {
		if c, ok := k.(Checker); ok {
			if secret, err := s.open(row); err == nil && c.Check(ctx, secret) == nil {
				s.log.Info("a job said its login was turned down; its provider still takes it", "job", uuidString(job.ID), "kind", row.Kind)
				return false, nil
			}
		}
	}
	err = s.locked(ctx, func(q *db.Queries) error {
		row, err := q.GetUserCredentialByID(ctx, job.CredentialID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		said := "Turned down while running a job"
		if reason = strings.TrimSpace(reason); reason != "" {
			said += ": " + clip(reason, 300)
		}
		if _, err := s.setStatus(ctx, q, row, StatusError, said, false); err != nil {
			return err
		}
		return emit(ctx, q, "credential.refused", row.UserID, map[string]any{"kind": row.Kind, "job_id": uuidString(job.ID)})
	})
	if err != nil {
		return false, err
	}
	s.changed()
	return true, nil
}

// Sensitive implements runners.Credentials.
func (s *Service) Sensitive(ctx context.Context, job db.Job) []string {
	if !job.CredentialID.Valid {
		return nil
	}
	row, err := db.New(s.pool).GetUserCredentialByID(ctx, job.CredentialID)
	if err != nil {
		return nil
	}
	k, ok := s.kinds.Get(row.Kind)
	if !ok {
		return nil
	}
	secret, err := s.open(row)
	if err != nil {
		return nil
	}
	return k.Sensitive(secret)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	v, _ := u.Value()
	s, _ := v.(string)
	return s
}
