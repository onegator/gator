package curator

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/brain"
	"github.com/onegator/gator/internal/server/conversations"
	pgstore "github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

// Defaults of when a conversation is curated. What a thread settled comes back as a proposal
// within Promise: DefaultQuiet of silence, a sweep at most DefaultEvery later, and the curation
// job (a cheap model, no tools) in what is left.
const (
	// Promise is how soon after a thread goes quiet what it settled is proposed.
	Promise = time.Hour
	// DefaultQuiet is how long a thread has to be silent.
	DefaultQuiet = 30 * time.Minute
	// DefaultEvery is how often the curator looks for quiet threads.
	DefaultEvery = time.Minute
	// DefaultMinMessages is how much has to have been said since the last curation.
	DefaultMinMessages = 4
	// MaxOpenPerConversation bounds what waits from one conversation: past it, nothing more is
	// proposed until a person has decided on what is there.
	MaxOpenPerConversation = 10
)

// CurationKind and CurationRole are the job a curation is: a transcript read by a model with no
// tools and no identity (jobs.kind summary, the runner's no-tools kind), as the curator.
const (
	CurationKind = "summary"
	CurationRole = "curator"
)

// store is what the curator reads and writes. *db.Queries is one.
type store interface {
	ConversationsDueForCuration(ctx context.Context, arg db.ConversationsDueForCurationParams) ([]db.ConversationsDueForCurationRow, error)
	ClaimCuration(ctx context.Context, arg db.ClaimCurationParams) (db.BrainCuration, error)
	SetCurationJob(ctx context.Context, arg db.SetCurationJobParams) error
	ReleaseCuration(ctx context.Context, conversationID pgtype.UUID) error
	GetCuration(ctx context.Context, conversationID pgtype.UUID) (db.BrainCuration, error)
	CountOpenProposalsFrom(ctx context.Context, conversationID pgtype.UUID) (int64, error)
	GetConversation(ctx context.Context, id pgtype.UUID) (db.Conversation, error)
	GetTaskConversation(ctx context.Context, taskID pgtype.UUID) (db.Conversation, error)
}

// txStore is what the curator writes in one transaction when a curation ends: the proposals and
// how far it has read, together, so that a redelivered event finds the curation done. *db.Queries
// is one.
type txStore interface {
	brain.Store
	CurationByJob(ctx context.Context, jobID pgtype.UUID) (db.BrainCuration, error)
	FinishCuration(ctx context.Context, conversationID pgtype.UUID) error
	FailCuration(ctx context.Context, conversationID pgtype.UUID) error
	CountOpenProposalsFrom(ctx context.Context, conversationID pgtype.UUID) (int64, error)
	MessageAtSeq(ctx context.Context, arg db.MessageAtSeqParams) (db.Message, error)
	GetConversation(ctx context.Context, id pgtype.UUID) (db.Conversation, error)
	GetComponent(ctx context.Context, id pgtype.UUID) (db.Component, error)
	GetJob(ctx context.Context, id pgtype.UUID) (db.Job, error)
}

// Tx runs fn in one transaction.
type Tx func(ctx context.Context, fn func(txStore) error) error

// PoolTx is a Tx over a pool.
func PoolTx(pool *pgxpool.Pool) Tx {
	return func(ctx context.Context, fn func(txStore) error) error {
		return pgstore.InTx(ctx, pool, func(q *db.Queries) error { return fn(q) })
	}
}

// Jobs queues a curation job on a task. The agents package provides one over the runner manager.
type Jobs interface {
	QueueCuration(ctx context.Context, task pgtype.UUID, instruction string) (pgtype.UUID, error)
}

// packer packs a conversation. conversations.Packer is one.
type packer interface {
	Pack(ctx context.Context, conv db.Conversation, ask db.Message) (conversations.Pack, error)
}

// openness says whether a conversation is open to its whole project. conversations.Access is one.
type openness interface {
	OpenToProject(ctx context.Context, conv db.Conversation) (bool, error)
}

// proposer stores proposals within a transaction the curator holds. brain.Service is one.
type proposer interface {
	RecordIn(ctx context.Context, st brain.Store, d brain.Draft, by brain.Author, status string) (db.BrainEntry, error)
}

// Curator decides when a conversation is read, asks for the reading, and proposes what it found.
type Curator struct {
	store   store
	tx      Tx
	jobs    Jobs
	packer  packer
	open    openness
	brain   proposer
	extract Extract
	log     *slog.Logger
	// Quiet and MinMessages say when a thread is due, MaxOpen how many of its proposals may wait.
	Quiet       time.Duration
	MinMessages int
	MaxOpen     int
	now         func() time.Time
}

// New builds it.
func New(st store, tx Tx, jobs Jobs, p packer, open openness, b proposer, log *slog.Logger) *Curator {
	if log == nil {
		log = slog.Default()
	}
	return &Curator{store: st, tx: tx, jobs: jobs, packer: p, open: open, brain: b, log: log,
		Quiet: DefaultQuiet, MinMessages: DefaultMinMessages, MaxOpen: MaxOpenPerConversation, now: time.Now}
}

// Run sweeps for quiet threads every so often until ctx ends.
func (c *Curator) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := c.Sweep(ctx); err != nil && ctx.Err() == nil {
				c.log.Warn("curating quiet conversations", "err", err)
			}
		}
	}
}

// Sweep asks for a curation of every task thread that has gone quiet with enough said. It
// answers how many it asked for.
func (c *Curator) Sweep(ctx context.Context) (int, error) {
	due, err := c.store.ConversationsDueForCuration(ctx, db.ConversationsDueForCurationParams{
		QuietBefore: pgtype.Timestamptz{Time: c.now().Add(-c.Quiet), Valid: true}, MinMessages: int64(c.MinMessages)})
	if err != nil {
		return 0, err
	}
	asked := 0
	for _, row := range due {
		conv, err := c.store.GetConversation(ctx, row.ID)
		if err != nil {
			continue
		}
		ok, err := c.Curate(ctx, conv)
		if err != nil {
			c.log.Warn("asking for a curation", "conversation", brain.UUIDString(conv.ID), "err", err)
			continue
		}
		if ok {
			asked++
		}
	}
	return asked, nil
}

// Curate asks for a curation of conv up to its newest message, unless one is being written, the
// conversation is private (what is said there stays out of a job the project reads), it has no
// task to do the job on, or what it proposed still waits.
func (c *Curator) Curate(ctx context.Context, conv db.Conversation) (bool, error) {
	if !conv.TaskID.Valid {
		return false, nil
	}
	if open, err := c.open.OpenToProject(ctx, conv); err != nil || !open {
		return false, err
	}
	if n, err := c.store.CountOpenProposalsFrom(ctx, conv.ID); err != nil || n >= int64(c.MaxOpen) {
		return false, err
	}
	// Nothing said since the last curation: nothing to read (a task closing right after one).
	if prev, err := c.store.GetCuration(ctx, conv.ID); err == nil && conv.LastSeq <= prev.UptoSeq {
		return false, nil
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	claim, err := c.store.ClaimCuration(ctx, db.ClaimCurationParams{ConversationID: conv.ID, Upto: conv.LastSeq})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // one is being written
	}
	if err != nil {
		return false, err
	}
	pack, err := c.packer.Pack(ctx, conv, db.Message{})
	if err != nil {
		_ = c.store.ReleaseCuration(ctx, conv.ID)
		return false, err
	}
	job, err := c.jobs.QueueCuration(ctx, conv.TaskID, c.extract.Instruction(pack, claim.UptoSeq))
	if err != nil {
		_ = c.store.ReleaseCuration(ctx, conv.ID)
		return false, err
	}
	return true, c.store.SetCurationJob(ctx, db.SetCurationJobParams{ConversationID: conv.ID, JobID: job})
}

// Events are what the curator follows: a curation job ending, and a task closing (its thread is
// read then, however recently someone spoke).
func (c *Curator) Events() []string { return []string{"job.finished", "task.closed"} }

// Handle implements conversations.Handler.
func (c *Curator) Handle(ctx context.Context, e db.EventsAfterRow) error {
	if !e.AggregateID.Valid {
		return nil
	}
	switch e.Type {
	case "task.closed":
		conv, err := c.store.GetTaskConversation(ctx, e.AggregateID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = c.Curate(ctx, conv)
		return err
	case "job.finished":
		return c.finished(ctx, e.AggregateID)
	}
	return nil
}

// finished proposes what a curation job found and records how far it read, in one transaction:
// a redelivered event finds the curation done. A job that failed is tried again later, later each
// time; a candidate the Brain refuses is left out, not the whole curation.
func (c *Curator) finished(ctx context.Context, jobID pgtype.UUID) error {
	return c.tx(ctx, func(st txStore) error {
		claim, err := st.CurationByJob(ctx, jobID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // not a curation, or one already done
		}
		if err != nil {
			return err
		}
		job, err := st.GetJob(ctx, jobID)
		if err != nil {
			return err
		}
		if job.Status != "done" {
			return st.FailCuration(ctx, claim.ConversationID)
		}
		var receipt struct {
			Summary string `json:"summary"`
		}
		_ = json.Unmarshal(job.Receipt, &receipt)
		conv, err := st.GetConversation(ctx, claim.ConversationID)
		if err != nil {
			return err
		}
		open, err := st.CountOpenProposalsFrom(ctx, conv.ID)
		if err != nil {
			return err
		}
		room := c.MaxOpen - int(open)
		for _, cand := range c.extract.Candidates(receipt.Summary) {
			if room <= 0 {
				break // what was proposed before still waits for a person
			}
			ok, err := c.propose(ctx, st, conv, job, cand)
			if err != nil {
				return err
			}
			if ok {
				room--
			}
		}
		return st.FinishCuration(ctx, conv.ID)
	})
}

// propose stores one candidate: about the task's component when the task has one (so that the
// next task about it is handed it), else about the project, with the message it was settled in.
// ok is false for a candidate the Brain refuses.
func (c *Curator) propose(ctx context.Context, st txStore, conv db.Conversation, job db.Job, cand Candidate) (bool, error) {
	task, err := st.GetTask(ctx, conv.TaskID)
	if err != nil {
		return false, err
	}
	d := brain.Draft{Scope: brain.ScopeProject, Project: task.ProjectID, Kind: cand.Kind, Title: cand.Title, Content: cand.Content,
		Task: task.ID, Conversation: conv.ID, Source: brain.Source{Kind: brain.SourceMessage, ID: brain.UUIDString(conv.ID), Excerpt: cand.Quote},
		Links: []brain.Link{{TargetKind: "conversation", TargetID: brain.UUIDString(conv.ID)}, {TargetKind: "task", TargetID: brain.UUIDString(task.ID)}}}
	if msg, err := st.MessageAtSeq(ctx, db.MessageAtSeqParams{ConversationID: conv.ID, Seq: cand.Seq}); err == nil {
		d.Source.ID, d.Source.URL = brain.UUIDString(msg.ID), conversations.MessageLink(msg)
	}
	if task.ComponentID.Valid {
		if comp, err := st.GetComponent(ctx, task.ComponentID); err == nil && comp.Status == "approved" {
			d.Scope, d.Component = brain.ScopeComponent, comp.Key
			d.Links = append(d.Links, brain.Link{TargetKind: "component", TargetID: comp.Key})
		}
	}
	_, err = c.brain.RecordIn(ctx, st, d, brain.Author{Kind: brain.BySystem, ID: job.AgentID}, brain.StatusProposed)
	if brain.IsInvalid(err) {
		c.log.Info("a curation candidate left out", "conversation", brain.UUIDString(conv.ID), "title", cand.Title, "why", err)
		return false, nil
	}
	return err == nil, err
}
