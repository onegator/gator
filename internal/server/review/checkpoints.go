// Package review is what people see of an agent's work turn by turn and what they say about it:
// the checkpoints runners send after every turn (what the turn changed, as a diff) and the
// comments people leave on the lines of those diffs, which go back to the agent as its next task.
package review

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/proto"
	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/secrets"
	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

var (
	// ErrNotFound answers a checkpoint or a comment that does not exist.
	ErrNotFound = errors.New("not found")
	// ErrInvalid answers a request that cannot be carried out as asked. It is a refusal
	// (runners.Refusal): a runner that sent it is not asked to send it again.
	ErrInvalid error = refusal("invalid")
	// ErrForbidden answers what the caller may see but not do.
	ErrForbidden = errors.New("not allowed")
)

// refusal is an error a resend will not fix.
type refusal string

func (r refusal) Error() string { return string(r) }

// Refusal implements runners.Refusal.
func (refusal) Refusal() {}

// Blobs keeps diffs where attachments are kept. conversations.BlobStore is one; without one the
// diffs stay in the database.
type Blobs interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	Open(ctx context.Context, key string) (io.ReadCloser, error)
}

// EventCheckpoint says a job's turn was checkpointed: its card shows what the job changed so far.
const EventCheckpoint = conversations.EventJobCheckpoint

// Checkpoints records the checkpoints runners send and reads their diffs back.
type Checkpoints struct {
	pool  *pgxpool.Pool
	blobs Blobs
}

// NewCheckpoints builds it; blobs may be nil.
func NewCheckpoints(pool *pgxpool.Pool, blobs Blobs) *Checkpoints {
	return &Checkpoints{pool: pool, blobs: blobs}
}

// Limits of what a checkpoint may say about itself.
const (
	maxRepoName = 200
	maxPath     = 4096
	maxSeq      = 10_000
)

// Record keeps one checkpoint of a job's worktree, as the runner that holds the job sent it. Its
// diffs are redacted like logs before anything keeps them. A checkpoint sent again is the one
// already there. It answers ErrInvalid for one the runner had no business sending: a job that
// reads (chat) has a read-only checkout and no checkpoints.
func (c *Checkpoints) Record(ctx context.Context, job db.Job, cp proto.Checkpoint) error {
	if err := validCheckpoint(job, cp); err != nil {
		return err
	}
	files, err := json.Marshal(nonNilFiles(cp.Files))
	if err != nil {
		return err
	}
	whole, err := json.Marshal(nonNilFiles(cp.WholeFiles))
	if err != nil {
		return err
	}
	diff, wholeDiff := []byte(secrets.RedactText(string(cp.Diff))), []byte(secrets.RedactText(string(cp.Whole)))
	in := db.InsertCheckpointParams{JobID: job.ID, TurnID: job.TurnID, Seq: int32(cp.Seq), Repo: cp.Repo,
		BaseCommit: cp.BaseCommit, HeadCommit: cp.HeadCommit, Files: files, DiffBytes: int64(len(diff)), Truncated: cp.Truncated,
		WholeFiles: whole, WholeBytes: int64(len(wholeDiff)), WholeTruncated: cp.WholeTruncated, TookMs: int32(min(cp.TookMS, 1<<30))}
	if c.blobs != nil {
		// The key is the checkpoint's own, of this attempt: a resend writes the same file again.
		prefix := fmt.Sprintf("checkpoints/%s/%d/%s/%d", uuidString(job.ID), job.Attempts, repoKey(cp.Repo), cp.Seq)
		turnKey, wholeKey := prefix+"/turn.diff", prefix+"/whole.diff"
		if err := c.blobs.Put(ctx, turnKey, bytes.NewReader(diff), int64(len(diff)), "text/x-diff"); err != nil {
			return fmt.Errorf("storing the diff: %w", err)
		}
		if err := c.blobs.Put(ctx, wholeKey, bytes.NewReader(wholeDiff), int64(len(wholeDiff)), "text/x-diff"); err != nil {
			return fmt.Errorf("storing the diff: %w", err)
		}
		in.DiffKey, in.WholeKey = &turnKey, &wholeKey
	} else {
		in.Diff, in.WholeDiff = diff, wholeDiff
	}
	return store.InTx(ctx, c.pool, func(q *db.Queries) error {
		row, err := q.InsertCheckpoint(ctx, in)
		if err != nil || !row.Inserted {
			return err
		}
		payload, _ := json.Marshal(map[string]any{"task_id": uuidString(job.TaskID), "checkpoint_id": uuidString(row.ID),
			"seq": cp.Seq, "repo": cp.Repo, "summary": cp.Summary(), "took_ms": cp.TookMS})
		_, err = q.InsertEvent(ctx, db.InsertEventParams{Type: EventCheckpoint, Aggregate: "job", AggregateID: job.ID, Payload: payload})
		return err
	})
}

func validCheckpoint(job db.Job, cp proto.Checkpoint) error {
	switch {
	case job.Tier == proto.TierChat:
		return fmt.Errorf("%w: a job that reads has no checkpoints", ErrInvalid)
	case cp.Seq < 1 || cp.Seq > maxSeq:
		return fmt.Errorf("%w: checkpoint %d", ErrInvalid, cp.Seq)
	case cp.Repo == "" || len(cp.Repo) > maxRepoName || !utf8.ValidString(cp.Repo):
		return fmt.Errorf("%w: repository %q", ErrInvalid, cp.Repo)
	case !commitID(cp.BaseCommit) || !commitID(cp.HeadCommit):
		return fmt.Errorf("%w: commits %q..%q", ErrInvalid, cp.BaseCommit, cp.HeadCommit)
	case len(cp.Diff) > proto.MaxDiffBytes || len(cp.Whole) > proto.MaxDiffBytes:
		return fmt.Errorf("%w: a diff over %d bytes", ErrInvalid, proto.MaxDiffBytes)
	case len(cp.Files) > proto.MaxCheckpointFiles || len(cp.WholeFiles) > proto.MaxCheckpointFiles:
		return fmt.Errorf("%w: more than %d files", ErrInvalid, proto.MaxCheckpointFiles)
	}
	for _, f := range append(append([]proto.FileChange{}, cp.Files...), cp.WholeFiles...) {
		if f.Path == "" || len(f.Path) > maxPath || !utf8.ValidString(f.Path) || f.Added < 0 || f.Removed < 0 {
			return fmt.Errorf("%w: file %q", ErrInvalid, f.Path)
		}
		switch f.Status {
		case proto.FileAdded, proto.FileModified, proto.FileDeleted:
		default:
			return fmt.Errorf("%w: file status %q", ErrInvalid, f.Status)
		}
	}
	return nil
}

func commitID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// repoKey is a repository's name as a storage key can carry it.
func repoKey(repo string) string {
	sum := sha256.Sum256([]byte(repo))
	return hex.EncodeToString(sum[:8])
}

func nonNilFiles(f []proto.FileChange) []proto.FileChange {
	if f == nil {
		return []proto.FileChange{}
	}
	return f
}

// Checkpoint is a turn's checkpoint, as people read it.
type Checkpoint = db.GetCheckpointRow

// List answers a job's checkpoints, turn by turn.
func (c *Checkpoints) List(ctx context.Context, job pgtype.UUID) ([]Checkpoint, error) {
	rows, err := db.New(c.pool).ListCheckpoints(ctx, job)
	if err != nil {
		return nil, err
	}
	out := make([]Checkpoint, len(rows))
	for i, r := range rows {
		out[i] = Checkpoint(r)
	}
	return out, nil
}

// Get answers one checkpoint.
func (c *Checkpoints) Get(ctx context.Context, id pgtype.UUID) (Checkpoint, error) {
	cp, err := db.New(c.pool).GetCheckpoint(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Checkpoint{}, ErrNotFound
	}
	return cp, err
}

// Which diff of a checkpoint: its turn's, or the job's up to it.
type Which int

const (
	TurnDiff Which = iota
	WholeDiff
)

// Diff answers a checkpoint's diff.
func (c *Checkpoints) Diff(ctx context.Context, id pgtype.UUID, which Which) ([]byte, error) {
	row, err := db.New(c.pool).CheckpointDiff(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	key, inline := row.DiffKey, row.Diff
	if which == WholeDiff {
		key, inline = row.WholeKey, row.WholeDiff
	}
	if key == nil {
		return inline, nil
	}
	if c.blobs == nil {
		return nil, fmt.Errorf("the diff is in storage this server is not configured with")
	}
	r, err := c.blobs.Open(ctx, *key)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(io.LimitReader(r, proto.MaxDiffBytes+1))
}

// JobDiff answers a job's diff over turns from..to (its seq numbers, both included) of one
// repository: exact from the first turn (the job's diff up to to), the turns' diffs put together
// file by file otherwise (Compose). to = 0 is the job's last turn.
func (c *Checkpoints) JobDiff(ctx context.Context, job pgtype.UUID, repo string, from, to int) ([]byte, error) {
	all, err := c.List(ctx, job)
	if err != nil {
		return nil, err
	}
	var cps []Checkpoint
	for _, cp := range all {
		if repo == "" {
			repo = cp.Repo
		}
		if cp.Repo == repo {
			cps = append(cps, cp)
		}
	}
	if len(cps) == 0 {
		return nil, ErrNotFound
	}
	if to == 0 {
		to = int(cps[len(cps)-1].Seq)
	}
	if from == 0 {
		from = 1
	}
	if from > to {
		return nil, fmt.Errorf("%w: from %d is after to %d", ErrInvalid, from, to)
	}
	var parts [][]byte
	for _, cp := range cps {
		seq := int(cp.Seq)
		if seq == to && from == 1 {
			return c.Diff(ctx, cp.ID, WholeDiff)
		}
		if seq < from || seq > to {
			continue
		}
		d, err := c.Diff(ctx, cp.ID, TurnDiff)
		if err != nil {
			return nil, err
		}
		parts = append(parts, d)
	}
	if len(parts) == 0 {
		return nil, ErrNotFound
	}
	return Compose(parts), nil
}

// Changes are what a job changed so far: files, lines added and removed, for its card.
type Changes struct {
	Files   int `json:"files"`
	Added   int `json:"added"`
	Removed int `json:"removed"`
}

// ChangesOf counts a list of files as JobChanges answers it.
func ChangesOf(files []byte) Changes {
	var fs []proto.FileChange
	_ = json.Unmarshal(files, &fs)
	out := Changes{Files: len(fs)}
	for _, f := range fs {
		out.Added += f.Added
		out.Removed += f.Removed
	}
	return out
}

func (c Changes) String() string {
	word := "files"
	if c.Files == 1 {
		word = "file"
	}
	return fmt.Sprintf("%d %s, +%d −%d", c.Files, word, c.Added, c.Removed)
}

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
