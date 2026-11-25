// Package conversations is where the people of a project talk: channels, direct messages,
// threads under one message, and the thread every task has. Work shows up in a conversation as
// cards (a task, a decision waiting at a gate, a job, an artifact) and a message can be made into
// a task. The process stays the backbone: a card points at a task or a job and its buttons call
// the same endpoints as before; this package never moves a task by itself.
//
// The package is split by responsibility: Access decides who may do what in a conversation,
// Directory keeps conversations and their members, Messages posts and reads messages, Threads
// finds or opens the thread under a message or a task, Cards turns domain events into cards,
// Promoter makes a message into a task, Search finds messages. Each takes what it needs through
// its constructor.
package conversations

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/auth"
)

// Conversation kinds.
const (
	KindChannel = "channel"
	KindDM      = "dm"
	KindThread  = "thread"
	KindTask    = "task"
)

// Visibility of a channel.
const (
	VisibilityProject = "project"
	VisibilityPrivate = "private"
)

// Member kinds. Agents become members in phase 2; the model has room for them now.
const (
	MemberUser  = "user"
	MemberAgent = "agent"
)

// Author kinds of a message.
const (
	AuthorUser     = "user"
	AuthorAgent    = "agent"
	AuthorSystem   = "system"
	AuthorPlugin   = "plugin"
	AuthorExternal = "external"
)

// Notification levels of a member.
const (
	NotifyAll      = "all"
	NotifyMentions = "mentions"
	NotifyNone     = "none"
)

// Member roles in a conversation.
const (
	RoleOwner  = "owner"
	RoleMember = "member"
)

// DefaultChannel is the channel every project has from the start.
const DefaultChannel = "general"

// DM sizes, the caller included.
const (
	minDMMembers = 2
	maxDMMembers = 8
)

var (
	// ErrNotFound answers a conversation, message or file that does not exist or that the
	// caller may not read: a private conversation's existence is not given away.
	ErrNotFound = errors.New("not found")
	// ErrForbidden answers something the caller may see but not do.
	ErrForbidden = errors.New("not allowed in this conversation")
	// ErrInvalid answers a request that cannot be carried out as asked.
	ErrInvalid = errors.New("invalid")
	// ErrConflict answers a request that clashes with what is already there.
	ErrConflict = errors.New("conflict")
)

// found turns the store's "no rows" into ErrNotFound, at the edge where a lookup of something
// the caller asked for leaves this package.
func found(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

func conflict(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrConflict, fmt.Sprintf(format, args...))
}

// Member is one participant: a person or (from phase 2) an agent.
type Member struct {
	Kind string
	ID   pgtype.UUID
}

// memberOf answers who the caller is as a member. Only people are members in phase 1; runners
// and plugins act on conversations they may read but are in none.
func memberOf(p auth.Principal) (Member, bool) {
	if p.Kind == auth.KindUser && p.UserID.Valid {
		return Member{Kind: MemberUser, ID: p.UserID}, true
	}
	return Member{}, false
}

// Author is who a message is from.
type Author struct {
	Kind string
	ID   pgtype.UUID
}

// System is the author of cards and of what Gator says by itself.
var System = Author{Kind: AuthorSystem}

// authorOf answers who the caller writes as. A person writes as themselves and a plugin as the
// plugin; a runner has nothing to say in a conversation, and an agent speaks from phase 2.
func authorOf(p auth.Principal) (Author, bool) {
	switch {
	case p.Kind == auth.KindUser && p.UserID.Valid:
		return Author{Kind: AuthorUser, ID: p.UserID}, true
	case p.Kind == auth.KindPlugin && p.TokenID.Valid:
		return Author{Kind: AuthorPlugin, ID: p.TokenID}, true
	}
	return Author{}, false
}

// dmKey identifies a DM by its people, whatever order they were named in.
func dmKey(ids []pgtype.UUID) string {
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		keys = append(keys, uuidString(id))
	}
	slices.Sort(keys)
	return strings.Join(keys, ",")
}

func uuidString(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	b := u.Bytes
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func uuidStrings(ids []pgtype.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, uuidString(id))
	}
	return out
}

func ptr[T any](v T) *T { return &v }
