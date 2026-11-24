package agents

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/store/db"
)

// talkStore is what Talk reads. *db.Queries is one.
type talkStore interface {
	GetTurn(ctx context.Context, id pgtype.UUID) (db.AgentTurn, error)
	GetTask(ctx context.Context, id pgtype.UUID) (db.Task, error)
	GetAgent(ctx context.Context, id pgtype.UUID) (db.Agent, error)
	GetAgentByHandle(ctx context.Context, arg db.GetAgentByHandleParams) (db.Agent, error)
	GetConversation(ctx context.Context, id pgtype.UUID) (db.Conversation, error)
}

// taskThreads answers a task's thread. conversations.Threads is one.
type taskThreads interface {
	ForTask(ctx context.Context, task db.Task) (db.Conversation, error)
}

// proposer posts the tasks agents propose. conversations.Proposals is one.
type proposer interface {
	Propose(ctx context.Context, conv db.Conversation, in conversations.Proposal) (db.Message, error)
}

// Talk is what an agent says from inside its job (gator-cli say, thread show, task create): in
// the conversation its turn answers in, or for a phase job, its task's thread. What it says is
// its own message, in its turn, so a mention of another agent is a delegation in its chain.
type Talk struct {
	store     talkStore
	threads   taskThreads
	voice     speaker
	packer    packer
	proposals proposer
}

// NewTalk builds it.
func NewTalk(store talkStore, threads taskThreads, voice speaker, packer packer, proposals proposer) *Talk {
	return &Talk{store: store, threads: threads, voice: voice, packer: packer, proposals: proposals}
}

// mentionPattern finds @handles in what an agent writes. An agent's handle is unique where it is
// seen, so the words are enough, where a person's mention has to be picked from a list.
var mentionPattern = regexp.MustCompile(`(?:^|[^\w@./-])@([a-z0-9][a-z0-9-]{1,31})\b`)

// maxSay bounds one thing an agent says.
const maxSay = 20000

// Where is the conversation a job speaks in and the agent that speaks.
func (t *Talk) Where(ctx context.Context, job db.Job) (db.Conversation, db.Agent, error) {
	if !job.AgentID.Valid {
		return db.Conversation{}, db.Agent{}, ErrForbidden
	}
	agent, err := t.store.GetAgent(ctx, job.AgentID)
	if err != nil {
		return db.Conversation{}, db.Agent{}, err
	}
	if job.TurnID.Valid {
		turn, err := t.store.GetTurn(ctx, job.TurnID)
		if err != nil {
			return db.Conversation{}, db.Agent{}, err
		}
		conv, err := t.store.GetConversation(ctx, turn.ConversationID)
		return conv, agent, err
	}
	task, err := t.store.GetTask(ctx, job.TaskID)
	if err != nil {
		return db.Conversation{}, db.Agent{}, err
	}
	conv, err := t.threads.ForTask(ctx, task)
	return conv, agent, err
}

// Say posts what the agent says, asking the agents it mentions.
func (t *Talk) Say(ctx context.Context, job db.Job, body string) (db.Message, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return db.Message{}, ErrInvalid
	}
	if len(body) > maxSay {
		body = body[:maxSay] + "…"
	}
	conv, agent, err := t.Where(ctx, job)
	if err != nil {
		return db.Message{}, err
	}
	mentions, err := t.mentions(ctx, job.ProjectID, body)
	if err != nil {
		return db.Message{}, err
	}
	return t.voice.Say(ctx, conv, conversations.Speech{Agent: agent.ID, Turn: job.TurnID, Body: body, Mentions: mentions})
}

// mentions are the agents of the project a text names by handle.
func (t *Talk) mentions(ctx context.Context, project pgtype.UUID, body string) ([]conversations.Member, error) {
	seen := map[string]bool{}
	var out []conversations.Member
	for _, m := range mentionPattern.FindAllStringSubmatch(body, -1) {
		handle := m[1]
		if seen[handle] {
			continue
		}
		seen[handle] = true
		a, err := t.store.GetAgentByHandle(ctx, db.GetAgentByHandleParams{Handle: handle, ProjectID: project})
		if errors.Is(err, pgx.ErrNoRows) {
			continue // a word, or a person's name
		}
		if err != nil {
			return nil, err
		}
		out = append(out, conversations.Member{Kind: conversations.MemberAgent, ID: a.ID})
	}
	return out, nil
}

// Thread is the conversation the job speaks in, its latest messages up to limit (oldest first),
// as the agent's prompt carries them: more of it than the prompt had room for.
func (t *Talk) Thread(ctx context.Context, job db.Job, limit int) (db.Conversation, []conversations.Said, error) {
	conv, _, err := t.Where(ctx, job)
	if err != nil {
		return db.Conversation{}, nil, err
	}
	pack, err := t.packer.Pack(ctx, conv, db.Message{})
	if err != nil {
		return db.Conversation{}, nil, err
	}
	all := append(append([]conversations.Said{}, pack.Older...), pack.Recent...)
	if limit > 0 && len(all) > limit {
		all = all[len(all)-limit:]
	}
	return conv, all, nil
}

// ProposeTask puts a task the agent proposes in front of the people of its conversation.
func (t *Talk) ProposeTask(ctx context.Context, job db.Job, kind, title, description string) (db.Message, error) {
	conv, agent, err := t.Where(ctx, job)
	if err != nil {
		return db.Message{}, err
	}
	return t.proposals.Propose(ctx, conv, conversations.Proposal{Kind: kind, Title: title, Description: description, Agent: agent, Origin: job.ID})
}
