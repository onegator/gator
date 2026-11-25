package curator

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/brain"
	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/store/db"
)

// Defaults of when a conversation is curated.
const (
	// DefaultQuiet is how long a thread has to be silent.
	DefaultQuiet = 30 * time.Minute
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
	CurationByJob(ctx context.Context, jobID pgtype.UUID) (db.BrainCuration, error)
	FinishCuration(ctx context.Context, conversationID pgtype.UUID) error
	ReleaseCuration(ctx context.Context, conversationID pgtype.UUID) error
	CountOpenProposalsFrom(ctx context.Context, conversationID pgtype.UUID) (int64, error)
	MessageAtSeq(ctx context.Context, arg db.MessageAtSeqParams) (db.Message, error)
	GetConversation(ctx context.Context, id pgtype.UUID) (db.Conversation, error)
	GetTaskConversation(ctx context.Context, taskID pgtype.UUID) (db.Conversation, error)
	GetTask(ctx context.Context, id pgtype.UUID) (db.Task, error)
	GetComponent(ctx context.Context, id pgtype.UUID) (db.Component, error)
	GetJob(ctx context.Context, id pgtype.UUID) (db.Job, error)
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

// proposer stores proposals. brain.Service is one.
type proposer interface {
	Propose(ctx context.Context, d brain.Draft, by brain.Author) (db.BrainEntry, error)
}

// Curator decides when a conversation is read, asks for the reading, and proposes what it found.
type Curator struct {
	store   store
	jobs    Jobs
	packer  packer
	open    openness
	brain   proposer
	extract Extract
	log     *slog.Logger
	// Quiet and MinMessages say when a thread is due; zero takes the defaults.
	Quiet       time.Duration
	MinMessages int
	now         func() time.Time
}

// New builds it.
func New(st store, jobs Jobs, p packer, open openness, b proposer, log *slog.Logger) *Curator {
	if log == nil {
		log = slog.Default()
	}
	return &Curator{store: st, jobs: jobs, packer: p, open: open, brain: b, log: log,
		Quiet: DefaultQuiet, MinMessages: DefaultMinMessages, now: time.Now}
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
	if n, err := c.store.CountOpenProposalsFrom(ctx, conv.ID); err != nil || n >= MaxOpenPerConversation {
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

// finished proposes what a curation job found, and lets the conversation be read again later.
func (c *Curator) finished(ctx context.Context, jobID pgtype.UUID) error {
	claim, err := c.store.CurationByJob(ctx, jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // not a curation
	}
	if err != nil {
		return err
	}
	job, err := c.store.GetJob(ctx, jobID)
	if err != nil {
		return err
	}
	var receipt struct {
		Summary string `json:"summary"`
	}
	_ = json.Unmarshal(job.Receipt, &receipt)
	if job.Status != "done" {
		return c.store.ReleaseCuration(ctx, claim.ConversationID)
	}
	conv, err := c.store.GetConversation(ctx, claim.ConversationID)
	if err != nil {
		return err
	}
	for _, cand := range c.extract.Candidates(receipt.Summary) {
		if err := c.propose(ctx, conv, job, cand); err != nil {
			return err
		}
	}
	return c.store.FinishCuration(ctx, conv.ID)
}

// propose stores one candidate: about the task's component when the task has one (so that the
// next task about it is handed it), else about the project, with the message it was settled in.
func (c *Curator) propose(ctx context.Context, conv db.Conversation, job db.Job, cand Candidate) error {
	task, err := c.store.GetTask(ctx, conv.TaskID)
	if err != nil {
		return err
	}
	d := brain.Draft{Scope: brain.ScopeProject, Project: task.ProjectID, Kind: cand.Kind, Title: cand.Title, Content: cand.Content,
		Task: task.ID, Conversation: conv.ID, Source: brain.Source{Kind: brain.SourceMessage, ID: brain.UUIDString(conv.ID), Excerpt: cand.Quote},
		Links: []brain.Link{{TargetKind: "conversation", TargetID: brain.UUIDString(conv.ID)}, {TargetKind: "task", TargetID: brain.UUIDString(task.ID)}}}
	if msg, err := c.store.MessageAtSeq(ctx, db.MessageAtSeqParams{ConversationID: conv.ID, Seq: cand.Seq}); err == nil {
		d.Source.ID, d.Source.URL = brain.UUIDString(msg.ID), conversations.MessageLink(msg)
	}
	if task.ComponentID.Valid {
		if comp, err := c.store.GetComponent(ctx, task.ComponentID); err == nil && comp.Status == "approved" {
			d.Scope, d.Component = brain.ScopeComponent, comp.Key
			d.Links = append(d.Links, brain.Link{TargetKind: "component", TargetID: comp.Key})
		}
	}
	_, err = c.brain.Propose(ctx, d, brain.Author{Kind: brain.BySystem, ID: job.AgentID})
	return err
}
