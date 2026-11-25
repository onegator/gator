package bridges

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/conversations"
	"github.com/onegator/gator/internal/server/store/db"
	"github.com/onegator/gator/internal/server/words"
	"github.com/onegator/gator/plugin"
)

// FindOrCreate is conversation.find_or_create: the conversation an outside place is connected
// to, connecting it the first time.
func (s *Service) FindOrCreate(ctx context.Context, b Bridge, p plugin.ConversationFindOrCreateParams) (plugin.ConversationFindOrCreateResult, error) {
	if !validRef(p.ExternalRef) {
		return plugin.ConversationFindOrCreateResult{}, invalid("external_ref is 1–%d characters of one line", maxRef)
	}
	if ref, err := s.q.GetExternalConversation(ctx, db.GetExternalConversationParams{ProjectID: b.Project, Provider: b.Provider,
		ExternalRef: p.ExternalRef}); err == nil {
		return plugin.ConversationFindOrCreateResult{ConversationID: conversations.UUIDString(ref.ConversationID)}, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return plugin.ConversationFindOrCreateResult{}, err
	}
	var conv db.Conversation
	var err error
	switch p.Kind {
	case plugin.BridgeChannel:
		conv, _, err = s.deps.Writer.BridgeChannel(ctx, b.Project, p.Name, p.Topic)
		if errors.Is(err, conversations.ErrInvalid) || errors.Is(err, conversations.ErrConflict) {
			return plugin.ConversationFindOrCreateResult{}, invalid("%v", err)
		}
	case plugin.BridgeThread:
		conv, err = s.threadUnder(ctx, b, p.ParentRef)
	case plugin.BridgeTask:
		conv, err = s.taskThread(ctx, b, p.TaskID)
	default:
		return plugin.ConversationFindOrCreateResult{}, invalid("kind is channel, thread or task, not %q", p.Kind)
	}
	if err != nil {
		return plugin.ConversationFindOrCreateResult{}, err
	}
	if open, err := s.deps.Rights.OpenToProject(ctx, conv); err != nil {
		return plugin.ConversationFindOrCreateResult{}, err
	} else if !open {
		return plugin.ConversationFindOrCreateResult{}, forbidden("a private conversation is never connected to the outside")
	}
	// A conversation has one home on each provider: a second place pointing at it (a Slack thread
	// whose root was mirrored, a reply in it) finds it without a mapping of its own.
	if _, err := s.q.GetConversationRefOf(ctx, db.GetConversationRefOfParams{Provider: b.Provider, ConversationID: conv.ID}); err == nil {
		return plugin.ConversationFindOrCreateResult{ConversationID: conversations.UUIDString(conv.ID)}, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return plugin.ConversationFindOrCreateResult{}, err
	}
	created := true
	if _, err := s.q.InsertExternalConversation(ctx, db.InsertExternalConversationParams{ProjectID: b.Project, Provider: b.Provider,
		ExternalRef: p.ExternalRef, ConversationID: conv.ID}); errors.Is(err, pgx.ErrNoRows) {
		created = false // connected a moment ago by a delivery racing this one
	} else if err != nil {
		return plugin.ConversationFindOrCreateResult{}, err
	}
	return plugin.ConversationFindOrCreateResult{ConversationID: conversations.UUIDString(conv.ID), Created: created}, nil
}

// threadUnder is the thread under a message the bridge brought in or mirrored.
func (s *Service) threadUnder(ctx context.Context, b Bridge, parentRef string) (db.Conversation, error) {
	if !validRef(parentRef) {
		return db.Conversation{}, invalid("a thread names the message it hangs under (parent_ref)")
	}
	ref, err := s.q.GetExternalMessage(ctx, db.GetExternalMessageParams{ProjectID: b.Project, Provider: b.Provider, ExternalRef: parentRef})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Conversation{}, notFound("no message %q came through this bridge", parentRef)
	}
	if err != nil {
		return db.Conversation{}, err
	}
	msg, err := s.q.GetMessage(ctx, ref.MessageID)
	if err != nil {
		return db.Conversation{}, err
	}
	if _, err := s.bridged(ctx, b, msg.ConversationID); err != nil {
		return db.Conversation{}, err
	}
	return s.deps.Threads.Under(ctx, msg)
}

// taskThread is the thread of a task of the bridge's project.
func (s *Service) taskThread(ctx context.Context, b Bridge, id string) (db.Conversation, error) {
	tid, ok := words.UUID(id)
	if !ok {
		return db.Conversation{}, invalid("task_id %q is not a uuid", id)
	}
	task, err := s.q.GetTask(ctx, tid)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && task.ProjectID != b.Project) {
		return db.Conversation{}, notFound("task %s not found in this project", id)
	}
	if err != nil {
		return db.Conversation{}, err
	}
	return s.deps.Threads.ForTask(ctx, task)
}

// bridged answers a conversation of the bridge's project that the bridge connected: itself, or the
// conversation its root message lives in. Anything else looks like it does not exist.
func (s *Service) bridged(ctx context.Context, b Bridge, id pgtype.UUID) (db.Conversation, error) {
	conv, err := s.q.GetConversation(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && conv.ProjectID != b.Project) {
		return db.Conversation{}, notFound("conversation not found")
	}
	if err != nil {
		return db.Conversation{}, err
	}
	root, err := s.q.GetAccessRoot(ctx, conv.ID)
	if err != nil {
		return db.Conversation{}, err
	}
	refs, err := s.q.ListBridgesOfConversation(ctx, db.ListBridgesOfConversationParams{ConversationID: conv.ID, RootID: root.ID})
	if err != nil {
		return db.Conversation{}, err
	}
	for _, r := range refs {
		if r.Provider == b.Provider && r.ProjectID == b.Project {
			return conv, nil
		}
	}
	return db.Conversation{}, notFound("conversation not found")
}

// Post is message.post: what somebody said outside, as the person their account is linked to when
// that person may write here, else as the outside identity.
func (s *Service) Post(ctx context.Context, b Bridge, p plugin.MessagePostParams) (plugin.MessagePostResult, error) {
	id, ok := words.UUID(p.ConversationID)
	if !ok {
		return plugin.MessagePostResult{}, invalid("conversation_id %q is not a uuid", p.ConversationID)
	}
	if !validRef(p.ExternalRef) {
		return plugin.MessagePostResult{}, invalid("external_ref is 1–%d characters of one line", maxRef)
	}
	conv, err := s.bridged(ctx, b, id)
	if err != nil {
		return plugin.MessagePostResult{}, err
	}
	if conv.ArchivedAt.Valid {
		return plugin.MessagePostResult{}, forbidden("the conversation is archived")
	}
	if open, err := s.deps.Rights.OpenToProject(ctx, conv); err != nil {
		return plugin.MessagePostResult{}, err
	} else if !open {
		return plugin.MessagePostResult{}, forbidden("a private conversation is never connected to the outside")
	}
	who, err := s.identity(ctx, b, p.Author)
	if err != nil {
		return plugin.MessagePostResult{}, err
	}
	author := conversations.Author{Kind: conversations.AuthorExternal, ID: who.ID}
	if who.UserID.Valid {
		ok, err := s.deps.Rights.UserCanWrite(ctx, conv, who.UserID)
		if err != nil {
			return plugin.MessagePostResult{}, err
		}
		if ok {
			author = conversations.Author{Kind: conversations.AuthorUser, ID: who.UserID}
		}
	}
	mentions, err := s.agents(ctx, b, p.Mentions)
	if err != nil {
		return plugin.MessagePostResult{}, err
	}
	if len(mentions) > 0 && author.Kind == conversations.AuthorExternal && b.Provider != ProviderA2A {
		if _, err := s.q.GetExternalMessage(ctx, db.GetExternalMessageParams{ProjectID: b.Project, Provider: b.Provider, ExternalRef: p.ExternalRef}); errors.Is(err, pgx.ErrNoRows) &&
			!s.asks.Allow(words.UUIDString(who.ID)) {
			return plugin.MessagePostResult{}, forbidden("too many asks from this account; wait a minute")
		}
	}
	msg, replayed, err := s.deps.Writer.Post(ctx, conversations.BridgedPost{Conversation: conv, Author: author, Body: p.Body,
		Mentions: mentions, Provider: b.Provider, ExternalRef: p.ExternalRef})
	if errors.Is(err, conversations.ErrInvalid) {
		return plugin.MessagePostResult{}, invalid("%v", err)
	}
	if err != nil {
		return plugin.MessagePostResult{}, err
	}
	return plugin.MessagePostResult{MessageID: conversations.UUIDString(msg.ID), Replayed: replayed, AuthorKind: msg.AuthorKind}, nil
}

// identity is the outside account as this project and bridge know it, made the first time it
// speaks.
func (s *Service) identity(ctx context.Context, b Bridge, a plugin.ExternalAuthor) (db.ExternalIdentity, error) {
	ext := strings.TrimSpace(a.ExternalID)
	if ext == "" || len(ext) > maxExternalID || !validRef(ext) {
		return db.ExternalIdentity{}, invalid("an author's external_id is 1–%d characters of one line", maxExternalID)
	}
	return s.q.UpsertExternalIdentity(ctx, db.UpsertExternalIdentityParams{ProjectID: b.Project, Provider: b.Provider,
		ExternalID: ext, DisplayName: words.OneLine(a.DisplayName, maxDisplayName)})
}

// agents are the agents of the bridge's project that a message names by handle.
func (s *Service) agents(ctx context.Context, b Bridge, handles []string) ([]conversations.Member, error) {
	if len(handles) > 10 {
		return nil, invalid("a message asks at most 10 agents")
	}
	seen := map[[16]byte]bool{}
	var out []conversations.Member
	for _, h := range handles {
		h = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(h), "@"))
		if h == "" {
			continue
		}
		a, err := s.q.GetAgentByHandle(ctx, db.GetAgentByHandleParams{Handle: h, ProjectID: b.Project})
		if errors.Is(err, pgx.ErrNoRows) {
			continue // a word that looked like a handle
		}
		if err != nil {
			return nil, err
		}
		if !seen[a.ID.Bytes] {
			seen[a.ID.Bytes] = true
			out = append(out, conversations.Member{Kind: conversations.MemberAgent, ID: a.ID})
		}
	}
	return out, nil
}

// Update is message.update: a message the bridge brought in, edited outside by its author.
func (s *Service) Update(ctx context.Context, b Bridge, p plugin.MessageUpdateParams) error {
	if !validRef(p.ExternalRef) {
		return invalid("external_ref is 1–%d characters of one line", maxRef)
	}
	who, err := s.q.FindExternalIdentity(ctx, db.FindExternalIdentityParams{ProjectID: b.Project, Provider: b.Provider,
		ExternalID: strings.TrimSpace(p.Author.ExternalID)})
	if errors.Is(err, pgx.ErrNoRows) {
		return forbidden("only who wrote a message changes it")
	}
	if err != nil {
		return err
	}
	by := []conversations.Author{{Kind: conversations.AuthorExternal, ID: who.ID}}
	if who.UserID.Valid {
		by = append(by, conversations.Author{Kind: conversations.AuthorUser, ID: who.UserID})
	}
	_, err = s.deps.Writer.Edit(ctx, b.Project, b.Provider, p.ExternalRef, p.Body, by...)
	switch {
	case errors.Is(err, conversations.ErrForbidden):
		return forbidden("only who wrote a message changes it")
	case errors.Is(err, conversations.ErrNotFound):
		return notFound("no message %q came in through this bridge", p.ExternalRef)
	case errors.Is(err, conversations.ErrInvalid):
		return invalid("%v", err)
	}
	return err
}
