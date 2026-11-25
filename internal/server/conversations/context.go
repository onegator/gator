package conversations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/names"
	"github.com/onegator/gator/internal/server/store/db"
)

// The conversation package: what an agent is told about the conversation it answers in. It is
// built like a prompt is read — the task first (the message that asks and who asks it), then the
// background (a summary of the older part), then the latest messages word for word as far as the
// budget goes, and the task again, briefly, at the end. Words written outside the workspace (a
// plugin, a bridge) are marked untrusted: they are data to weigh, never instructions.

// Budgets of a pack, in bytes of text.
const (
	// DefaultRecentBytes is how much of the latest conversation is carried word for word.
	DefaultRecentBytes = 24000
	// maxMessageBytes bounds one message in the pack, so one long paste does not crowd out the rest.
	maxMessageBytes = 6000
	// maxPackMessages is how far back a pack looks at all.
	maxPackMessages = 200
	// extractRunes is how much of each older message the stand-in background quotes.
	extractRunes = 160
	// maxExtractBytes bounds the stand-in background.
	maxExtractBytes = 6000
)

// Said is one message as a pack carries it.
type Said struct {
	Seq  int64
	Who  string // "Ada", "@reviewer", "Gator", "plugin github"
	Body string
	// Untrusted is a message from outside the workspace.
	Untrusted bool
}

// Pack is a conversation as an agent is told it.
type Pack struct {
	// Ask is the message that asks, and Asker who wrote it.
	Ask   Said
	Asker string
	// Where names the conversation: "#general", "a thread in #general", "a DM".
	Where string
	// Background summarises what came before Recent; empty when everything fits.
	Background string
	// Recent are the latest messages, oldest first, word for word; the ask is not among them.
	Recent []Said
	// Older are the messages before Recent that the background stands for, oldest first. A
	// summarizer reads them; the agent does not.
	Older []Said
	// SummarisedUpTo is the last seq the stored summary covers, 0 for none; Stale says the
	// background is partly a stand-in for messages the summary does not cover yet.
	SummarisedUpTo int64
	Stale          bool
	// Marker fences the outside words of this pack, here and wherever the pack is written out.
	Marker Marker
	// Private is a conversation not open to the whole project (a private channel, a DM): what is
	// said in it stays out of anything the project's people read — the record of what a job was
	// handed, a summary job's instruction.
	Private bool
	// Notes are the review comments people sent with the ask or the latest messages: comments on
	// lines of the agent's changes, with the code they are about.
	Notes []Note
}

// Note is one review comment as an agent is told it: the file, the lines (on the side of the diff
// they number: old before the change, new after), the code there as the diff showed it, and what
// the person said about it.
type Note struct {
	Author    string
	Path      string
	Side      string
	LineStart int
	LineEnd   int
	Snippet   string
	Body      string
}

// Bytes is how much of the prompt the pack takes.
func (p Pack) Bytes() int {
	n := len(p.Ask.Body) + len(p.Background)
	for _, s := range p.Recent {
		n += len(s.Body)
	}
	return n
}

// packStore is what the packer reads. *db.Queries is one.
type packStore interface {
	GetConversation(ctx context.Context, id pgtype.UUID) (db.Conversation, error)
	GetAccessRoot(ctx context.Context, id pgtype.UUID) (db.Conversation, error)
	GetMessage(ctx context.Context, id pgtype.UUID) (db.Message, error)
	ListMessagesBefore(ctx context.Context, arg db.ListMessagesBeforeParams) ([]db.Message, error)
	GetConversationSummary(ctx context.Context, conversationID pgtype.UUID) (db.ConversationSummary, error)
	GetUser(ctx context.Context, id pgtype.UUID) (db.User, error)
	GetAgent(ctx context.Context, id pgtype.UUID) (db.Agent, error)
	GetExternalIdentity(ctx context.Context, id pgtype.UUID) (db.ExternalIdentity, error)
	ListReviewNotes(ctx context.Context, messageIds []pgtype.UUID) ([]db.ListReviewNotesRow, error)
}

// Packer builds the conversation package of a turn. It only reads: keeping the summary fresh is
// the Summaries' work, from what the pack says is stale.
type Packer struct {
	store       packStore
	recentBytes int
}

// NewPacker builds it; recentBytes 0 takes DefaultRecentBytes.
func NewPacker(store packStore, recentBytes int) *Packer {
	if recentBytes <= 0 {
		recentBytes = DefaultRecentBytes
	}
	return &Packer{store: store, recentBytes: recentBytes}
}

// Pack is conv as told to an agent asked by ask. A thread starts with the message it hangs under.
func (p *Packer) Pack(ctx context.Context, conv db.Conversation, ask db.Message) (Pack, error) {
	names := newNamer(p.store)
	root, err := p.rootOf(ctx, conv)
	if err != nil {
		return Pack{}, err
	}
	out := Pack{Where: where(root, conv), Marker: NewMarker(), Private: !openToProject(root)}
	if out.Ask, err = names.said(ctx, ask, maxMessageBytes); err != nil {
		return Pack{}, err
	}
	out.Asker = out.Ask.Who
	history, err := p.history(ctx, conv, ask)
	if err != nil {
		return Pack{}, err
	}
	// The newest first, word for word, until the budget is spent; the rest is background.
	left, cut := p.recentBytes, len(history)
	for cut > 0 {
		s, err := names.said(ctx, history[cut-1], maxMessageBytes)
		if err != nil {
			return Pack{}, err
		}
		if len(s.Body) > left && cut < len(history) {
			break
		}
		left -= len(s.Body)
		out.Recent = append([]Said{s}, out.Recent...)
		cut--
	}
	for _, m := range history[:cut] {
		s, err := names.said(ctx, m, maxMessageBytes)
		if err != nil {
			return Pack{}, err
		}
		out.Older = append(out.Older, s)
	}
	if out.Notes, err = p.notes(ctx, ask, history[cut:]); err != nil {
		return Pack{}, err
	}
	return out, p.background(ctx, conv, &out)
}

// maxNotes bounds the review comments one pack carries.
const maxNotes = 50

// notes are the review comments sent with the ask and the messages carried word for word.
func (p *Packer) notes(ctx context.Context, ask db.Message, recent []db.Message) ([]Note, error) {
	ids := []pgtype.UUID{ask.ID}
	for _, m := range recent {
		if m.AuthorKind == AuthorUser {
			ids = append(ids, m.ID)
		}
	}
	rows, err := p.store.ListReviewNotes(ctx, ids)
	if err != nil {
		return nil, err
	}
	var out []Note
	for _, r := range rows {
		if len(out) == maxNotes {
			break
		}
		out = append(out, Note{Author: r.Author, Path: r.Path, Side: r.Side, LineStart: int(r.LineStart), LineEnd: int(r.LineEnd),
			Snippet: r.Snippet, Body: r.Body})
	}
	return out, nil
}

// history is what was said in conv, oldest first, but the ask itself: the message a thread hangs
// under, then the conversation as it is now — with what a person added after asking, before the
// agent started — without what is deleted and the answers still being written.
func (p *Packer) history(ctx context.Context, conv db.Conversation, ask db.Message) ([]db.Message, error) {
	rows, err := p.store.ListMessagesBefore(ctx, db.ListMessagesBeforeParams{ConversationID: conv.ID, BeforeSeq: conv.LastSeq + 1, MaxRows: maxPackMessages})
	if err != nil {
		return nil, err
	}
	reverse(rows)
	var out []db.Message
	if conv.ParentMessageID.Valid && conv.ParentMessageID != ask.ID {
		root, err := p.store.GetMessage(ctx, conv.ParentMessageID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if err == nil && !root.DeletedAt.Valid {
			root.Seq = 0 // before the thread's first reply
			out = append(out, root)
		}
	}
	for _, m := range rows {
		if m.DeletedAt.Valid || m.Status == StatusStreaming || m.ID == ask.ID {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// background fills in what came before the recent messages: the stored summary where it covers
// them, and the first words of each message it does not cover yet.
func (p *Packer) background(ctx context.Context, conv db.Conversation, out *Pack) error {
	if len(out.Older) == 0 {
		return nil
	}
	sum, err := p.store.GetConversationSummary(ctx, conv.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var b strings.Builder
	if strings.TrimSpace(sum.Summary) != "" {
		out.SummarisedUpTo = sum.UptoSeq
		b.WriteString(strings.TrimSpace(sum.Summary))
	}
	var extract strings.Builder
	for _, s := range out.Older {
		covered := sum.UptoSeq > 0 && s.Seq <= sum.UptoSeq // a thread's root, seq 0, comes before everything
		if covered && !s.Untrusted {
			continue
		}
		if !s.Untrusted {
			out.Stale = true // outside words are never summarised, so they never make it stale
		}
		if extract.Len() > maxExtractBytes {
			continue
		}
		line := strings.ReplaceAll(clip(s.Body, extractRunes), "\n", " ")
		if s.Untrusted {
			fmt.Fprintf(&extract, "- %s\n", out.Marker.Fence(s.Who, line))
			continue
		}
		fmt.Fprintf(&extract, "- %s: %s\n", s.Who, line)
	}
	if extract.Len() > 0 {
		if b.Len() > 0 {
			b.WriteString("\n\nSince then (the first words of each message):\n")
		} else {
			b.WriteString("The first words of each earlier message:\n")
		}
		b.WriteString(strings.TrimRight(extract.String(), "\n"))
	}
	out.Background = b.String()
	return nil
}

// rootOf is the conversation that decides who reads conv.
func (p *Packer) rootOf(ctx context.Context, conv db.Conversation) (db.Conversation, error) {
	if !conv.ParentMessageID.Valid {
		return conv, nil
	}
	return p.store.GetAccessRoot(ctx, conv.ID)
}

// where names a conversation for an agent.
func where(root, conv db.Conversation) string {
	name := "a conversation"
	switch {
	case root.Kind == KindChannel && root.Name != nil:
		name = "#" + *root.Name
	case root.Kind == KindDM:
		name = "a DM"
	case root.Kind == KindTask:
		name = "a task's thread"
	}
	if conv.ParentMessageID.Valid {
		return "a thread in " + name // whether it is a task's the brief says apart
	}
	return name
}

// namer says who wrote what, asking the store once per author.
type namer struct {
	store packStore
	seen  map[string]string
	// outside are the authors whose words came from outside the workspace though they are
	// members: remote agents, whose answers are another system's.
	outside map[string]bool
}

func newNamer(store packStore) *namer {
	return &namer{store: store, seen: map[string]string{}, outside: map[string]bool{}}
}

func (n *namer) said(ctx context.Context, m db.Message, max int) (Said, error) {
	who, err := n.who(ctx, m)
	if err != nil {
		return Said{}, err
	}
	body := m.Body
	if m.Kind == messageCard {
		body = "[card] " + body
	}
	if len(body) > max {
		body = truncateUTF8(body, max) + "\n… [cut]"
	}
	untrusted := m.AuthorKind == AuthorPlugin || m.AuthorKind == AuthorExternal || n.outside[m.AuthorKind+":"+uuidString(m.AuthorID)]
	return Said{Seq: m.Seq, Who: who, Body: body, Untrusted: untrusted}, nil
}

func (n *namer) who(ctx context.Context, m db.Message) (string, error) {
	key := m.AuthorKind + ":" + uuidString(m.AuthorID)
	if s, ok := n.seen[key]; ok {
		return s, nil
	}
	s := m.AuthorKind
	switch m.AuthorKind {
	case AuthorSystem:
		s = "Gator"
	case AuthorUser:
		u, err := n.store.GetUser(ctx, m.AuthorID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
		s = "a person"
		if err == nil && strings.TrimSpace(u.Name) != "" {
			s = u.Name
		}
	case AuthorAgent:
		a, err := n.store.GetAgent(ctx, m.AuthorID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
		s = "an agent"
		if err == nil {
			s = "@" + a.Handle
			if a.Kind == AgentRemote {
				s += " (an agent outside Gator)"
				n.outside[key] = true
			}
		}
	case AuthorPlugin:
		s = "a plugin"
	case AuthorExternal:
		// The bridge it came through, never the name it gave itself there: a display name is the
		// outside's words too, and it is not fenced.
		s = "someone outside the workspace"
		if id, err := n.store.GetExternalIdentity(ctx, m.AuthorID); err == nil && names.Valid(id.Provider) {
			s = "someone on " + id.Provider + ", outside the workspace"
		} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
	}
	n.seen[key] = s
	return s, nil
}

// truncateUTF8 cuts s to at most n bytes without splitting a character.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
