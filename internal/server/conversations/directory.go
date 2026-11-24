package conversations

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

// Directory keeps conversations and who is in them: channels, DMs, joining and leaving, the
// lists a sidebar shows. Messages are not its business (see Messages).
type Directory struct {
	pool   *pgxpool.Pool
	access *Access
	roles  RoleResolver
}

// NewDirectory builds the directory.
func NewDirectory(pool *pgxpool.Pool, access *Access, roles RoleResolver) *Directory {
	return &Directory{pool: pool, access: access, roles: roles}
}

// Detail is one conversation as its reader sees it.
type Detail struct {
	Conversation db.Conversation
	Members      []db.ListConversationMembersRow
	Rights       Rights
	// Me is the caller's own membership, when they are in it.
	Me *db.ListConversationMembersRow
}

// Listed is one row of a conversation list, with the caller's unread counts.
type Listed struct {
	Conversation   db.Conversation
	Joined         bool
	LastReadSeq    int64
	Notify         string
	Unread         int64
	UnreadMentions int64
}

// NewChannel describes a channel to create.
type NewChannel struct {
	Name         string
	Topic        string
	Visibility   string // project (default) | private
	ComponentKey string
}

// Patch is a change to a conversation; nil fields stay as they are.
type Patch struct {
	Name     *string
	Topic    *string
	Archived *bool
}

// MemberChange is a change to a membership; nil fields stay as they are.
type MemberChange struct {
	Notify *string
	Role   *string
}

var channelName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,79}$`)

// normalizeChannelName lowercases a name and drops a leading '#'; it must then be 1–80 of
// a–z, 0–9, '-' and '_'.
func normalizeChannelName(name string) (string, error) {
	n := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(name), "#"))
	if !channelName.MatchString(n) {
		return "", invalid("a channel name is 1–80 lowercase letters, digits, '-' or '_', starting with a letter or digit")
	}
	return n, nil
}

// CreateChannel opens a channel in a project; its creator owns it. Only a person who may write
// in the project does.
func (d *Directory) CreateChannel(ctx context.Context, p auth.Principal, project pgtype.UUID, in NewChannel) (db.Conversation, error) {
	me, ok := memberOf(p)
	if !ok {
		return db.Conversation{}, ErrForbidden
	}
	role, err := d.roles.ProjectRole(ctx, p, project)
	if err != nil {
		return db.Conversation{}, err
	}
	if role < auth.RoleMember {
		return db.Conversation{}, ErrForbidden
	}
	name, err := normalizeChannelName(in.Name)
	if err != nil {
		return db.Conversation{}, err
	}
	visibility := in.Visibility
	if visibility == "" {
		visibility = VisibilityProject
	}
	if visibility != VisibilityProject && visibility != VisibilityPrivate {
		return db.Conversation{}, invalid("visibility is project or private")
	}
	var conv db.Conversation
	err = store.InTx(ctx, d.pool, func(q *db.Queries) error {
		var err error
		conv, err = q.CreateConversation(ctx, db.CreateConversationParams{
			ProjectID: project, Kind: KindChannel, Name: &name, Topic: strings.TrimSpace(in.Topic), Visibility: visibility,
			ComponentKey: optional(in.ComponentKey), CreatedByKind: AuthorUser, CreatedBy: me.ID,
		})
		if isUniqueViolation(err) {
			return conflict("this project already has a channel called #%s", name)
		}
		if err != nil {
			return err
		}
		if _, err := q.AddConversationMember(ctx, db.AddConversationMemberParams{
			ConversationID: conv.ID, MemberKind: me.Kind, MemberID: me.ID, Role: RoleOwner, Notify: NotifyMentions,
		}); err != nil {
			return err
		}
		return emit(ctx, q, EventConversationCreated, conv.ID, map[string]any{"kind": conv.Kind, "projectId": uuidString(project)})
	})
	return conv, err
}

// EnsureDefaultChannels gives a project its #general, if it has none yet, and puts people in it.
// Safe to repeat.
func (d *Directory) EnsureDefaultChannels(ctx context.Context, project pgtype.UUID, people ...pgtype.UUID) (db.Conversation, error) {
	var conv db.Conversation
	err := store.InTx(ctx, d.pool, func(q *db.Queries) error {
		created, err := q.CreateChannelIfMissing(ctx, db.CreateChannelIfMissingParams{ProjectID: project, Name: DefaultChannel, Topic: ""})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			if conv, err = q.GetChannelByName(ctx, db.GetChannelByNameParams{ProjectID: project, Name: DefaultChannel}); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			conv = created
		}
		for _, person := range people {
			if err := q.JoinDefaultChannel(ctx, db.JoinDefaultChannelParams{ProjectID: project, UserID: person, Name: DefaultChannel}); err != nil {
				return err
			}
		}
		if created.ID.Valid {
			return emit(ctx, q, EventConversationCreated, conv.ID, map[string]any{"kind": conv.Kind, "projectId": uuidString(project)})
		}
		return nil
	})
	return conv, err
}

// JoinDefaultChannels puts a person who joined a project into its #general.
func (d *Directory) JoinDefaultChannels(ctx context.Context, project, user pgtype.UUID) error {
	_, err := d.EnsureDefaultChannels(ctx, project, user)
	return err
}

// OpenDM answers the DM between the caller and these people, opening it when they have none:
// the same people in the same scope always get the same DM. A DM inside a project is only for
// people of that project; one outside is for anyone in the workspace.
func (d *Directory) OpenDM(ctx context.Context, p auth.Principal, people []pgtype.UUID, project pgtype.UUID) (db.Conversation, bool, error) {
	me, ok := memberOf(p)
	if !ok {
		return db.Conversation{}, false, ErrForbidden
	}
	ids := unique(append([]pgtype.UUID{me.ID}, people...))
	if len(ids) < minDMMembers || len(ids) > maxDMMembers {
		return db.Conversation{}, false, invalid("a DM has %d to %d people, you included", minDMMembers, maxDMMembers)
	}
	if project.Valid {
		role, err := d.roles.ProjectRole(ctx, p, project)
		if err != nil {
			return db.Conversation{}, false, err
		}
		if role < auth.RoleMember {
			return db.Conversation{}, false, ErrForbidden
		}
	}
	q := db.New(d.pool)
	for _, id := range ids[1:] {
		u, err := q.GetUser(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Conversation{}, false, invalid("no such person: %s", uuidString(id))
		}
		if err != nil {
			return db.Conversation{}, false, err
		}
		if project.Valid {
			role, err := d.roles.ProjectRole(ctx, principalOf(u), project)
			if err != nil {
				return db.Conversation{}, false, err
			}
			if role < auth.RoleViewer {
				return db.Conversation{}, false, invalid("%s is not in this project", u.Name)
			}
		}
	}
	key := dmKey(ids)
	var conv db.Conversation
	created := false
	err := store.InTx(ctx, d.pool, func(q *db.Queries) error {
		var err error
		conv, err = q.CreateDMIfMissing(ctx, db.CreateDMIfMissingParams{ProjectID: project, DmKey: key, CreatedBy: me.ID})
		if errors.Is(err, pgx.ErrNoRows) {
			conv, err = q.GetDM(ctx, db.GetDMParams{ProjectID: project, DmKey: key})
			return err
		}
		if err != nil {
			return err
		}
		created = true
		for _, id := range ids {
			if _, err := q.AddConversationMember(ctx, db.AddConversationMemberParams{
				ConversationID: conv.ID, MemberKind: MemberUser, MemberID: id, Role: RoleMember, Notify: NotifyAll,
			}); err != nil {
				return err
			}
		}
		payload := map[string]any{"kind": conv.Kind}
		if project.Valid {
			payload["projectId"] = uuidString(project)
		}
		return emit(ctx, q, EventConversationCreated, conv.ID, payload)
	})
	return conv, created, err
}

// Get answers a conversation with its members and what the caller may do in it.
func (d *Directory) Get(ctx context.Context, p auth.Principal, id pgtype.UUID) (Detail, error) {
	conv, rights, err := d.access.RightsByID(ctx, id, p)
	if err != nil {
		return Detail{}, err
	}
	if !rights.Read {
		return Detail{}, ErrNotFound
	}
	return d.detail(ctx, conv, rights, p)
}

// DetailOf is Get for a conversation already loaded and checked.
func (d *Directory) DetailOf(ctx context.Context, conv db.Conversation, p auth.Principal) (Detail, error) {
	rights, err := d.access.Rights(ctx, conv, p)
	if err != nil {
		return Detail{}, err
	}
	if !rights.Read {
		return Detail{}, ErrNotFound
	}
	return d.detail(ctx, conv, rights, p)
}

func (d *Directory) detail(ctx context.Context, conv db.Conversation, rights Rights, p auth.Principal) (Detail, error) {
	members, err := db.New(d.pool).ListConversationMembers(ctx, conv.ID)
	if err != nil {
		return Detail{}, err
	}
	out := Detail{Conversation: conv, Members: members, Rights: rights}
	if me, ok := memberOf(p); ok {
		for i := range members {
			if members[i].MemberKind == me.Kind && members[i].MemberID == me.ID {
				out.Me = &members[i]
			}
		}
	}
	return out, nil
}

// Update renames, re-topics, archives or brings back a conversation. Renaming and archiving are
// a moderator's, and only of a channel; the topic is anyone's who may write. #general keeps its
// name and is never archived: tasks post their cards there.
func (d *Directory) Update(ctx context.Context, p auth.Principal, id pgtype.UUID, in Patch) (db.Conversation, error) {
	conv, rights, err := d.access.RightsByID(ctx, id, p)
	if err != nil {
		return db.Conversation{}, err
	}
	if !rights.Read {
		return db.Conversation{}, ErrNotFound
	}
	params := db.UpdateConversationParams{ID: conv.ID}
	if in.Name != nil || in.Archived != nil {
		if !rights.Moderate {
			return db.Conversation{}, ErrForbidden
		}
		if conv.Kind != KindChannel {
			return db.Conversation{}, invalid("only a channel is renamed or archived")
		}
		if conv.Name != nil && *conv.Name == DefaultChannel {
			return db.Conversation{}, conflict("#%s is every project's; it keeps its name and stays open", DefaultChannel)
		}
	}
	if in.Name != nil {
		name, err := normalizeChannelName(*in.Name)
		if err != nil {
			return db.Conversation{}, err
		}
		params.Name = &name
	}
	if in.Topic != nil {
		if !rights.Write {
			return db.Conversation{}, ErrForbidden
		}
		params.Topic = ptr(strings.TrimSpace(*in.Topic))
	}
	params.Archived = in.Archived
	var out db.Conversation
	err = store.InTx(ctx, d.pool, func(q *db.Queries) error {
		var err error
		out, err = q.UpdateConversation(ctx, params)
		if isUniqueViolation(err) {
			return conflict("this project already has a channel called #%s", *params.Name)
		}
		if err != nil {
			return err
		}
		return emit(ctx, q, EventConversationUpdated, out.ID, map[string]any{"archived": out.ArchivedAt.Valid})
	})
	return out, err
}

// SetMember adds someone to a conversation or changes their membership. Anyone who may read a
// conversation joins it; adding someone else takes the right to write, and that person must be
// able to read it once in. How one is notified is one's own to choose; who owns a conversation
// is a moderator's. A DM keeps the people it was opened with.
func (d *Directory) SetMember(ctx context.Context, p auth.Principal, id pgtype.UUID, m Member, ch MemberChange) (db.ConversationMember, error) {
	conv, rights, err := d.access.RightsByID(ctx, id, p)
	if err != nil {
		return db.ConversationMember{}, err
	}
	if !rights.Read {
		return db.ConversationMember{}, ErrNotFound
	}
	if m.Kind != MemberUser {
		return db.ConversationMember{}, invalid("only people join conversations until agents do")
	}
	if ch.Notify != nil && !validNotify(*ch.Notify) {
		return db.ConversationMember{}, invalid("notify is all, mentions or none")
	}
	if ch.Role != nil && *ch.Role != RoleOwner && *ch.Role != RoleMember {
		return db.ConversationMember{}, invalid("role is owner or member")
	}
	me, _ := memberOf(p)
	self := me.ID.Valid && me.ID == m.ID
	switch {
	case ch.Role != nil && !rights.Moderate:
		return db.ConversationMember{}, ErrForbidden
	case !self && ch.Notify != nil:
		return db.ConversationMember{}, ErrForbidden // how someone hears about things is theirs
	case !self && !rights.Write:
		return db.ConversationMember{}, ErrForbidden
	}
	q := db.New(d.pool)
	existing, err := q.GetConversationMember(ctx, db.GetConversationMemberParams{ConversationID: conv.ID, MemberKind: m.Kind, MemberID: m.ID})
	in := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return db.ConversationMember{}, err
	}
	if !in {
		if conv.Kind == KindDM {
			return db.ConversationMember{}, conflict("a DM keeps the people it was opened with; open another for more")
		}
		if !self {
			ok, err := d.access.UserCanRead(ctx, conv, m.ID)
			if err != nil {
				return db.ConversationMember{}, err
			}
			// A private channel is read only by its members, so its own rule cannot ask that
			// question; the person must at least be able to read the project.
			if !ok && !(conv.Visibility == VisibilityPrivate && d.inProject(ctx, conv, m.ID)) {
				return db.ConversationMember{}, invalid("that person cannot read this conversation")
			}
		}
	}
	var out db.ConversationMember
	err = store.InTx(ctx, d.pool, func(q *db.Queries) error {
		before, joined := existing, false
		if !in {
			added, err := q.AddConversationMember(ctx, db.AddConversationMemberParams{
				ConversationID: conv.ID, MemberKind: m.Kind, MemberID: m.ID, Role: RoleMember, Notify: NotifyMentions,
			})
			switch {
			case err == nil:
				before, joined = added, true
			case errors.Is(err, pgx.ErrNoRows): // joined a moment ago, by another request
				if before, err = q.GetConversationMember(ctx, db.GetConversationMemberParams{ConversationID: conv.ID, MemberKind: m.Kind, MemberID: m.ID}); err != nil {
					return err
				}
			default:
				return err
			}
		}
		out = before
		if ch.Notify != nil || ch.Role != nil {
			var err error
			if out, err = q.UpdateConversationMember(ctx, db.UpdateConversationMemberParams{
				ConversationID: conv.ID, MemberKind: m.Kind, MemberID: m.ID, Notify: ch.Notify, Role: ch.Role,
			}); err != nil {
				return err
			}
		}
		if !joined && out.Notify == before.Notify && out.Role == before.Role {
			return nil
		}
		change := "updated"
		if joined {
			change = "joined"
		}
		return emit(ctx, q, EventMemberChanged, conv.ID, map[string]any{"member": memberPayload(m), "change": change, "role": out.Role})
	})
	return out, err
}

// RemoveMember takes someone out of a conversation: oneself by leaving, anyone else by a
// moderator. Nobody leaves a DM; it is the conversation of the people it was opened with.
func (d *Directory) RemoveMember(ctx context.Context, p auth.Principal, id pgtype.UUID, m Member) error {
	conv, rights, err := d.access.RightsByID(ctx, id, p)
	if err != nil {
		return err
	}
	if !rights.Read {
		return ErrNotFound
	}
	me, _ := memberOf(p)
	if !(me.ID.Valid && me.ID == m.ID && me.Kind == m.Kind) && !rights.Moderate {
		return ErrForbidden
	}
	if conv.Kind == KindDM {
		return conflict("a DM keeps the people it was opened with")
	}
	return store.InTx(ctx, d.pool, func(q *db.Queries) error {
		n, err := q.RemoveConversationMember(ctx, db.RemoveConversationMemberParams{ConversationID: conv.ID, MemberKind: m.Kind, MemberID: m.ID})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		var removed []pgtype.UUID
		if m.Kind == MemberUser {
			removed = append(removed, m.ID)
		}
		return emit(ctx, q, EventMemberChanged, conv.ID, map[string]any{"member": memberPayload(m), "change": "left"}, removed...)
	})
}

// ListFilter narrows a project's conversation list.
type ListFilter struct {
	Kind string // channel when empty
	// Archived includes archived conversations.
	Archived bool
	// Name keeps channels whose name contains it.
	Name string
}

// ListProject answers a project's conversations of one kind that the caller may read, with their
// unread counts. The caller has already been found able to read the project.
func (d *Directory) ListProject(ctx context.Context, p auth.Principal, project pgtype.UUID, f ListFilter) ([]Listed, error) {
	if f.Kind == "" {
		f.Kind = KindChannel
	}
	if !validKind(f.Kind) {
		return nil, invalid("kind is channel, dm, thread or task")
	}
	me, _ := memberOf(p)
	params := db.ListProjectConversationsParams{UserID: me.ID, ProjectID: project, Kind: f.Kind, IncludeArchived: f.Archived}
	if name := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(f.Name), "#")); name != "" {
		params.NameLike = ptr("%" + likeEscaper.Replace(name) + "%")
	}
	rows, err := db.New(d.pool).ListProjectConversations(ctx, params)
	if err != nil {
		return nil, err
	}
	role, err := d.roles.ProjectRole(ctx, p, project)
	if err != nil {
		return nil, err
	}
	out := make([]Listed, 0, len(rows))
	for _, r := range rows {
		ok, err := d.access.readableListed(ctx, r.Conversation, p, role, r.Joined)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, listed(r.Conversation, r.Joined, r.LastReadSeq, r.Notify, r.UnreadMentions))
		}
	}
	return out, nil
}

// likeEscaper makes a person's words literal inside a LIKE pattern.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// ListMine answers every conversation the caller is in and may still read, newest first; kind
// narrows it ("" for all).
func (d *Directory) ListMine(ctx context.Context, p auth.Principal, kind string) ([]Listed, error) {
	me, ok := memberOf(p)
	if !ok {
		return []Listed{}, nil
	}
	if kind != "" && !validKind(kind) {
		return nil, invalid("kind is channel, dm, thread or task")
	}
	rows, err := db.New(d.pool).ListMemberConversations(ctx, db.ListMemberConversationsParams{UserID: me.ID, Kind: optional(kind)})
	if err != nil {
		return nil, err
	}
	roles := map[[16]byte]auth.Role{}
	out := make([]Listed, 0, len(rows))
	for _, r := range rows {
		role := auth.RoleNone
		if pid := r.Conversation.ProjectID; pid.Valid {
			var seen bool
			if role, seen = roles[pid.Bytes]; !seen {
				if role, err = d.roles.ProjectRole(ctx, p, pid); err != nil {
					return nil, err
				}
				roles[pid.Bytes] = role
			}
		}
		ok, err := d.access.readableListed(ctx, r.Conversation, p, role, true)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, listed(r.Conversation, true, r.LastReadSeq, r.Notify, r.UnreadMentions))
		}
	}
	return out, nil
}

func listed(c db.Conversation, joined bool, lastRead int64, notify string, mentions int64) Listed {
	l := Listed{Conversation: c, Joined: joined, LastReadSeq: lastRead, Notify: notify, UnreadMentions: mentions}
	if joined && c.LastSeq > lastRead {
		l.Unread = c.LastSeq - lastRead
	}
	return l
}

// inProject answers whether a person may read the conversation's project.
func (d *Directory) inProject(ctx context.Context, conv db.Conversation, user pgtype.UUID) bool {
	if !conv.ProjectID.Valid {
		return false
	}
	u, err := db.New(d.pool).GetUser(ctx, user)
	if err != nil {
		return false
	}
	role, err := d.roles.ProjectRole(ctx, principalOf(u), conv.ProjectID)
	return err == nil && role >= auth.RoleViewer
}

func validKind(k string) bool {
	return k == KindChannel || k == KindDM || k == KindThread || k == KindTask
}

func validNotify(n string) bool { return n == NotifyAll || n == NotifyMentions || n == NotifyNone }

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func unique(ids []pgtype.UUID) []pgtype.UUID {
	seen := map[[16]byte]bool{}
	out := make([]pgtype.UUID, 0, len(ids))
	for _, id := range ids {
		if id.Valid && !seen[id.Bytes] {
			seen[id.Bytes] = true
			out = append(out, id)
		}
	}
	return out
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// projectConfig is the part of process_config about conversations:
// {"conversations": {"task_channel": "work"}}.
type projectConfig struct {
	Conversations struct {
		// TaskChannel is the channel a new task's card goes to; #general when unset, gone,
		// archived or private.
		TaskChannel string `json:"task_channel"`
	} `json:"conversations"`
}

// taskChannelName reads the configured task channel; "" when there is none or it is no name.
func taskChannelName(processConfig []byte) string {
	var cfg projectConfig
	if len(processConfig) == 0 || json.Unmarshal(processConfig, &cfg) != nil || cfg.Conversations.TaskChannel == "" {
		return ""
	}
	name, err := normalizeChannelName(cfg.Conversations.TaskChannel)
	if err != nil {
		return ""
	}
	return name
}

// TaskChannel answers the channel where a project's new tasks show up: the one its
// process_config names (conversations.task_channel), or #general. A task belongs to the whole
// project, so only an open channel will do: a private one, like an archived one, is ignored.
func (d *Directory) TaskChannel(ctx context.Context, project pgtype.UUID) (db.Conversation, error) {
	q := db.New(d.pool)
	p, err := q.GetProject(ctx, project)
	if err != nil {
		return db.Conversation{}, err
	}
	if name := taskChannelName(p.ProcessConfig); name != "" {
		ch, err := q.GetChannelByName(ctx, db.GetChannelByNameParams{ProjectID: project, Name: name})
		switch {
		case err == nil && !ch.ArchivedAt.Valid && ch.Visibility == VisibilityProject:
			return ch, nil
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return db.Conversation{}, err
		}
	}
	return d.EnsureDefaultChannels(ctx, project)
}
