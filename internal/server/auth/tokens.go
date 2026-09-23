package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/store/db"
)

// ErrInvalidToken is returned for unknown, revoked or expired tokens.
var ErrInvalidToken = errors.New("invalid token")

const tokenPrefix = "gtr_"

// Tokens issues and verifies bearer tokens. Only a SHA-256 hash is stored.
type Tokens struct{ Pool *pgxpool.Pool }

// IssueParams describes a token to mint.
type IssueParams struct {
	Kind      Kind
	Scope     string
	Name      string
	UserID    pgtype.UUID
	ProjectID pgtype.UUID
	JobID     pgtype.UUID   // set for job tokens; the token dies with the job
	TTL       time.Duration // 0 = no expiry
}

// Issue mints a token and returns its plaintext exactly once.
func (t Tokens) Issue(ctx context.Context, p IssueParams) (plaintext string, rec db.ApiToken, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", db.ApiToken{}, err
	}
	plaintext = tokenPrefix + string(p.Kind) + "_" + base64.RawURLEncoding.EncodeToString(raw)
	var exp pgtype.Timestamptz
	if p.TTL > 0 {
		exp = pgtype.Timestamptz{Time: time.Now().Add(p.TTL), Valid: true}
	}
	rec, err = db.New(t.Pool).InsertToken(ctx, db.InsertTokenParams{
		Kind: string(p.Kind), Scope: p.Scope, Hash: hash(plaintext), UserID: p.UserID,
		ProjectID: p.ProjectID, Name: p.Name, ExpiresAt: exp, JobID: p.JobID,
	})
	return plaintext, rec, err
}

// Verify resolves a plaintext token to a principal.
func (t Tokens) Verify(ctx context.Context, plaintext string) (Principal, error) {
	if !strings.HasPrefix(plaintext, tokenPrefix) {
		return Principal{}, ErrInvalidToken
	}
	q := db.New(t.Pool)
	rec, err := q.GetTokenByHash(ctx, hash(plaintext))
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrInvalidToken
	}
	if err != nil {
		return Principal{}, err
	}
	_ = q.TouchToken(ctx, rec.ID)
	p := Principal{Kind: Kind(rec.Kind), TokenID: rec.ID, Scope: rec.Scope, ProjectID: rec.ProjectID, Name: rec.Name, JobID: rec.JobID}
	if p.Kind == KindUser {
		u, err := q.GetUser(ctx, rec.UserID)
		if err != nil {
			return Principal{}, fmt.Errorf("token user: %w", err)
		}
		p.UserID = u.ID
		p.WorkspaceRole = u.WorkspaceRole
		p.Name = u.Name
	}
	return p, nil
}

// IssueForJob mints the identity one job's agent speaks with. Its scope is the job itself, so
// a token that leaks out of a worktree can still only talk about the work it was given.
func (t Tokens) IssueForJob(ctx context.Context, jobID, projectID pgtype.UUID, ttl time.Duration) (string, error) {
	plaintext, _, err := t.Issue(ctx, IssueParams{
		Kind: KindJob, Scope: "job:" + uuidString(jobID), Name: "agent",
		ProjectID: projectID, JobID: jobID, TTL: ttl,
	})
	return plaintext, err
}

// RevokeForJob invalidates whatever the job was given, when the job is over.
func (t Tokens) RevokeForJob(ctx context.Context, jobID pgtype.UUID) error {
	return db.New(t.Pool).RevokeTokensForJob(ctx, jobID)
}

// Revoke invalidates a token.
func (t Tokens) Revoke(ctx context.Context, id pgtype.UUID) error {
	return db.New(t.Pool).RevokeToken(ctx, id)
}

func uuidString(u pgtype.UUID) string {
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func hash(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}
