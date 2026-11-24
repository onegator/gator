package conversations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/store/db"
)

// SummaryJobs queues the job that writes a conversation's summary, on the task the conversation
// belongs to, with the summarizer's role and policy (policy.roles.summarizer: a cheap model).
// The agents package provides one over the runner manager.
type SummaryJobs interface {
	QueueSummary(ctx context.Context, task pgtype.UUID, instruction string) (pgtype.UUID, error)
}

// Summaries keeps the background of long conversations: when a pack finds older messages the
// stored summary does not cover, a summary job is queued, one at a time per conversation, and
// what it writes replaces the summary when it is done. Until then the pack stands in with the
// first words of each message, so no turn waits for a summary.
type Summaries struct {
	pool *pgxpool.Pool
	jobs SummaryJobs
}

// NewSummaries builds it.
func NewSummaries(pool *pgxpool.Pool, jobs SummaryJobs) *Summaries {
	return &Summaries{pool: pool, jobs: jobs}
}

// maxSummaryInput bounds what one summary job reads.
const maxSummaryInput = 60000

// Refresh queues a newer summary of conv when p says its background is stale. It does nothing
// while one is being written, or when the summary already covers the older messages.
func (s *Summaries) Refresh(ctx context.Context, conv db.Conversation, task pgtype.UUID, p Pack) error {
	if !p.Stale || len(p.Older) == 0 || s.jobs == nil {
		return nil
	}
	upto := p.Older[len(p.Older)-1].Seq
	q := db.New(s.pool)
	prev, err := q.ClaimSummaryRefresh(ctx, db.ClaimSummaryRefreshParams{ConversationID: conv.ID, UptoSeq: &upto})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // one is being written, or this is covered
	}
	if err != nil {
		return err
	}
	job, err := s.jobs.QueueSummary(ctx, task, summaryInstruction(prev.Summary, p))
	if err != nil {
		_ = q.ReleaseSummaryRefresh(ctx, conv.ID)
		return err
	}
	return q.SetSummaryJob(ctx, db.SetSummaryJobParams{ConversationID: conv.ID, JobID: job})
}

// summaryInstruction is what the summarizer reads: the summary so far, and the messages after it.
func summaryInstruction(previous string, p Pack) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Write the background of this conversation (%s) up to the messages below.\n", p.Where)
	if previous = strings.TrimSpace(previous); previous != "" {
		b.WriteString("\nThe summary so far:\n\n")
		b.WriteString(previous)
		b.WriteString("\n\nThe messages after it:\n\n")
	} else {
		b.WriteString("\nThe messages:\n\n")
	}
	outside := 0
	for _, s := range p.Older {
		if previous != "" && p.SummarisedUpTo > 0 && s.Seq <= p.SummarisedUpTo {
			continue
		}
		if s.Untrusted {
			// Words from outside never pass through a summary: what it says comes back as trusted
			// background. The pack shows them apart, fenced, for as long as they are there.
			outside++
			continue
		}
		if b.Len() > maxSummaryInput {
			b.WriteString("… [older messages left out]\n")
			break
		}
		b.WriteString(p.Marker.Render(s))
		b.WriteString("\n")
	}
	if outside > 0 {
		fmt.Fprintf(&b, "\n(%d messages from outside the workspace are left out of this; they are shown apart.)\n", outside)
	}
	return b.String()
}

// summaryKind is the kind of job that writes a summary (jobs.kind).
const summaryKind = "summary"

// Handler records what a summary job wrote when it ends: done replaces the summary, anything else
// lets the next pack ask again. It reads job.finished.
func (s *Summaries) Handler() Handler {
	return HandlerFunc(func(ctx context.Context, e db.EventsAfterRow) error {
		q := db.New(s.pool)
		job, err := q.GetJob(ctx, e.AggregateID)
		if err != nil {
			return ignoreGone(err)
		}
		if job.Kind != summaryKind {
			return nil
		}
		var receipt struct {
			Summary string `json:"summary"`
			Usage   struct {
				Model string `json:"model"`
			} `json:"usage"`
		}
		_ = json.Unmarshal(job.Receipt, &receipt)
		if job.Status != "done" || strings.TrimSpace(receipt.Summary) == "" {
			return q.ReleaseSummaryJob(ctx, job.ID)
		}
		_, err = q.FinishSummaryRefresh(ctx, db.FinishSummaryRefreshParams{JobID: job.ID, Summary: strings.TrimSpace(receipt.Summary), Model: receipt.Usage.Model})
		return err
	})
}
