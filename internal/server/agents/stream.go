package agents

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/events"
	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

// hubFeed is where job output arrives as it happens. events.Hub is one.
type hubFeed interface {
	Subscribe(topics ...string) (<-chan events.Event, func())
}

// streamStore is what the streamer reads. *db.Queries is one.
type streamStore interface {
	GetTurnByJob(ctx context.Context, jobID pgtype.UUID) (db.AgentTurn, error)
	ListJobEvents(ctx context.Context, arg db.ListJobEventsParams) ([]db.JobEvent, error)
}

// rewriter rewrites an answer being written. conversations.Voice is one.
type rewriter interface {
	Rewrite(ctx context.Context, message pgtype.UUID, body, status string) (db.Message, bool, error)
}

// DefaultStreamEvery is how often an answer being written is brought up to date.
const DefaultStreamEvery = 500 * time.Millisecond

// Streamer writes a reply into the agent's message as the agent writes it: the text it says
// becomes the message's body, a few times a second at most (message.edited), and its tool calls
// move the job's card instead (job.progress). The job's events in the database are the truth, so
// output the hub dropped is caught up on the next flush; the end of the turn is the TurnTracker's.
type Streamer struct {
	feed  hubFeed
	store streamStore
	voice rewriter
	pool  *pgxpool.Pool
	every time.Duration
	log   *slog.Logger

	replies map[string]*streaming // by job id
	others  map[string]bool       // jobs that are no reply
}

// streaming is what is known of one reply being written.
type streaming struct {
	turn  db.AgentTurn
	after int64
	text  []string
	tools int
	dirty bool
}

// NewStreamer builds it; every 0 is DefaultStreamEvery.
func NewStreamer(feed hubFeed, store streamStore, voice rewriter, pool *pgxpool.Pool, every time.Duration, log *slog.Logger) *Streamer {
	if every <= 0 {
		every = DefaultStreamEvery
	}
	if log == nil {
		log = slog.Default()
	}
	return &Streamer{feed: feed, store: store, voice: voice, pool: pool, every: every, log: log,
		replies: map[string]*streaming{}, others: map[string]bool{}}
}

// Run streams until ctx ends.
func (s *Streamer) Run(ctx context.Context) {
	ch, stop := s.feed.Subscribe("*")
	defer stop()
	tick := time.NewTicker(s.every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-ch:
			switch e.Type {
			case "job.output":
				s.mark(ctx, e.AggregateID)
			case "job.finished":
				s.flushOne(ctx, e.AggregateID)
				delete(s.replies, e.AggregateID)
				delete(s.others, e.AggregateID)
			}
		case <-tick.C:
			for id := range s.replies {
				s.flushOne(ctx, id)
			}
		}
	}
}

// mark notes new output of a job, learning the first time whether it is a reply.
func (s *Streamer) mark(ctx context.Context, job string) {
	if s.others[job] {
		return
	}
	if r, ok := s.replies[job]; ok {
		r.dirty = true
		return
	}
	var id pgtype.UUID
	if id.Scan(job) != nil {
		return
	}
	turn, err := s.store.GetTurnByJob(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		s.others[job] = true
		return
	}
	if err != nil {
		s.log.Warn("streaming a reply", "job", job, "err", err)
		return
	}
	s.replies[job] = &streaming{turn: turn, dirty: true}
}

// flushOne brings one reply's message and card up to date with its job's events.
func (s *Streamer) flushOne(ctx context.Context, job string) {
	r, ok := s.replies[job]
	if !ok || !r.dirty {
		return
	}
	r.dirty = false
	tools := r.tools
	for {
		evs, err := s.store.ListJobEvents(ctx, db.ListJobEventsParams{JobID: r.turn.JobID, Seq: r.after, Limit: 500})
		if err != nil {
			s.log.Warn("reading a reply's output", "job", job, "err", err)
			r.dirty = true
			return
		}
		for _, e := range evs {
			r.after = e.Seq
			switch e.Type {
			case "text":
				var p struct {
					Text string `json:"text"`
				}
				if json.Unmarshal(e.Payload, &p) == nil && strings.TrimSpace(p.Text) != "" {
					r.text = append(r.text, p.Text)
				}
			case "tool_call":
				r.tools++
			}
		}
		if len(evs) < 500 {
			break
		}
	}
	if body := Answer(r.text); body != "" && r.turn.ReplyMessageID.Valid {
		msg, _, err := s.voice.Rewrite(ctx, r.turn.ReplyMessageID, body, conversations.StatusStreaming)
		if err != nil {
			s.log.Warn("writing a reply", "job", job, "err", err)
		} else if msg.Status != conversations.StatusStreaming {
			delete(s.replies, job) // its turn ended; the answer is written
		}
	}
	if r.tools != tools {
		s.progressed(ctx, r.turn)
	}
}

// progressed tells the job's card that the agent called more tools.
func (s *Streamer) progressed(ctx context.Context, turn db.AgentTurn) {
	payload, _ := json.Marshal(map[string]any{"turn_id": uuidText(turn.ID)})
	err := store.InTx(ctx, s.pool, func(q *db.Queries) error {
		_, err := q.InsertEvent(ctx, db.InsertEventParams{Type: conversations.EventJobProgress, Aggregate: "job", AggregateID: turn.JobID, Payload: payload})
		return err
	})
	if err != nil {
		s.log.Warn("telling a job's card of its progress", "job", uuidText(turn.JobID), "err", err)
	}
}

// digestFence opens the block a job ends with for the runner; people never read it.
const digestFence = "```gator-digest"

// Answer is what an agent has said so far, as its message reads: its texts in order, without the
// digest it ends with.
func Answer(texts []string) string {
	body := strings.TrimSpace(strings.Join(texts, "\n\n"))
	if i := strings.Index(body, digestFence); i >= 0 {
		body = strings.TrimSpace(body[:i])
	}
	return body
}
