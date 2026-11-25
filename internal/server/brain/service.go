package brain

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/onegator/gator/internal/server/store"
	"github.com/onegator/gator/internal/server/store/db"
)

// Store is where the Brain keeps its entries. *db.Queries is one.
type Store interface {
	InsertBrainEntry(ctx context.Context, arg db.InsertBrainEntryParams) (db.BrainEntry, error)
	GetBrainEntry(ctx context.Context, id pgtype.UUID) (db.BrainEntry, error)
	LatestBrainVersionByTitle(ctx context.Context, arg db.LatestBrainVersionByTitleParams) (db.BrainEntry, error)
	LatestBrainVersion(ctx context.Context, lineage pgtype.UUID) (db.BrainEntry, error)
	ListBrainLineage(ctx context.Context, lineage pgtype.UUID) ([]db.BrainEntry, error)
	SetBrainStatus(ctx context.Context, arg db.SetBrainStatusParams) (db.BrainEntry, error)
	SupersedeOtherVersions(ctx context.Context, arg db.SupersedeOtherVersionsParams) (int64, error)
	SupersedeBrainEntry(ctx context.Context, id pgtype.UUID) (db.BrainEntry, error)
	ArchiveBrainEntries(ctx context.Context, ids []pgtype.UUID) (int64, error)
	ListBrainProposals(ctx context.Context, projectID pgtype.UUID) ([]db.BrainEntry, error)
	InsertBrainLink(ctx context.Context, arg db.InsertBrainLinkParams) error
	ListBrainLinks(ctx context.Context, ids []pgtype.UUID) ([]db.BrainLink, error)
	ListBrainCitations(ctx context.Context, ids []pgtype.UUID) ([]db.ListBrainCitationsRow, error)
	InsertEvent(ctx context.Context, arg db.InsertEventParams) (int64, error)
	ListUserBrain(ctx context.Context, userID pgtype.UUID) ([]db.BrainEntry, error)
	DeleteUserBrain(ctx context.Context, userID pgtype.UUID) (int64, error)
	StaleBrainEntries(ctx context.Context, arg db.StaleBrainEntriesParams) ([]db.BrainEntry, error)
	NarrowLineageVisibility(ctx context.Context, arg db.NarrowLineageVisibilityParams) error
	NewestApprovedVersion(ctx context.Context, lineage pgtype.UUID) (int32, error)
	LockLineage(ctx context.Context, lineage pgtype.UUID) error
	targetStore
}

// Tx runs fn against one transaction of the store.
type Tx func(ctx context.Context, fn func(Store) error) error

// PoolTx is a Tx over a pool.
func PoolTx(pool *pgxpool.Pool) Tx {
	return func(ctx context.Context, fn func(Store) error) error {
		return store.InTx(ctx, pool, func(q *db.Queries) error { return fn(q) })
	}
}

// Draft is an entry before it is stored.
type Draft struct {
	Scope      string
	Kind       string
	Topic      string // a convention's shelf
	Title      string
	Content    string
	Visibility string // empty: the scope's own rule
	Confidence string // empty: stated
	// Where it belongs, as its scope needs: a project, a component of it, a person, an agent.
	Project   pgtype.UUID
	Component string
	User      pgtype.UUID
	Agent     pgtype.UUID
	Source    Source
	// Task and Conversation are where it came from; Private says the conversation is not open
	// to the whole project, which keeps the entry, while it is a proposal, to that conversation's
	// readers and its source without a quote.
	Task         pgtype.UUID
	Conversation pgtype.UUID
	Private      bool
	Position     int32 // 0: 100
	// CondensedFrom are the entries a condensed one replaces; approving it archives them.
	CondensedFrom []pgtype.UUID
	Links         []Link
	// Propose asks for a proposal even from someone who may approve.
	Propose bool
}

// Link is what an entry is about besides itself.
type Link struct {
	TargetKind string // entry | component | task | conversation
	TargetID   string
	Relation   string // relates | depends_on | contradicts | implements
}

// Author is who wrote an entry.
type Author struct {
	Kind string // ByUser | ByRunner | ByPlugin | BySystem
	ID   pgtype.UUID
}

// Change is a new version of an entry: only what is set changes.
type Change struct {
	Title, Content, Visibility *string
	Propose                    bool
}

// Verdicts a person gives a proposal or an entry.
const (
	VerdictApprove = "approve"
	VerdictReject  = "reject"
	VerdictArchive = "archive"
)

// Effect is what happens to an entry besides storing it, in the same transaction: a proposal is
// checked against what is approved (Contradictions.Link); approving one makes earlier versions
// stop being true, archives what a condensed entry replaces and supersedes a decision it
// contradicts. A new consequence is one more Effect in Hooks.
type Effect func(ctx context.Context, st Store, e db.BrainEntry) error

// Hooks are the effects of proposing and of approving, run in order after the built-in ones.
type Hooks struct {
	Proposed []Effect
	Approved []Effect
}

// Service keeps entries: writes and proposals, versions, verdicts, and what one entry is (its
// versions, links and uses). Who may do which is the Policy's; searching is the Finder's.
type Service struct {
	tx      Tx
	store   Store
	policy  Policy
	sources Sources
	targets Targets
	hooks   Hooks
}

// NewService builds it. Approving an entry always makes its earlier versions stop being true and
// archives what a condensed entry replaces; hooks add to that.
func NewService(tx Tx, st Store, policy Policy, sources Sources, hooks Hooks) *Service {
	if sources == nil {
		sources = DefaultSources()
	}
	hooks.Approved = append([]Effect{supersedeEarlier, archiveCondensed}, hooks.Approved...)
	return &Service{tx: tx, store: st, policy: policy, sources: sources, targets: DefaultTargets(), hooks: hooks}
}

// Policy is the rules the service writes by.
func (s *Service) Policy() Policy { return s.policy }

// Write stores what r writes: approved straight away when r may approve it and did not ask for
// a proposal, a proposal otherwise. An agent's entry is always a proposal.
func (s *Service) Write(ctx context.Context, r Reader, d Draft) (db.BrainEntry, error) {
	d, err := s.prepare(d)
	if err != nil {
		return db.BrainEntry{}, err
	}
	rights, err := s.policy.Rights(ctx, r, d.probe())
	if err != nil {
		return db.BrainEntry{}, err
	}
	if !rights.Propose && !rights.Approve {
		return db.BrainEntry{}, ErrForbidden
	}
	if err := s.targets.check(ctx, s.store, s.policy, &r, d.probe(), d.Links); err != nil {
		return db.BrainEntry{}, err
	}
	by := Author{Kind: ByUser, ID: r.User}
	if r.IsJob() {
		by = Author{Kind: ByRunner, ID: r.Job}
		if d.Source.Kind == SourceManual {
			d.Source = Source{Kind: SourceJob, ID: UUIDString(r.Job)}
		}
	}
	status := StatusProposed
	if rights.Approve && !d.Propose && !r.IsJob() {
		status = StatusApproved
	}
	return s.put(ctx, s.tx, d, by, status, r.User, pgtype.UUID{})
}

// Propose stores a proposal from a part of Gator that has already decided it may make one: the
// curator, a plugin, a finished job's digest.
func (s *Service) Propose(ctx context.Context, d Draft, by Author) (db.BrainEntry, error) {
	d, err := s.prepare(d)
	if err != nil {
		return db.BrainEntry{}, err
	}
	return s.put(ctx, s.tx, d, by, StatusProposed, pgtype.UUID{}, pgtype.UUID{})
}

// Record stores what Gator itself knows and needs nobody's approval for — a person's focus, worked
// out from their work — with the status given.
func (s *Service) Record(ctx context.Context, d Draft, by Author, status string) (db.BrainEntry, error) {
	d, err := s.prepare(d)
	if err != nil {
		return db.BrainEntry{}, err
	}
	return s.put(ctx, s.tx, d, by, status, pgtype.UUID{}, pgtype.UUID{})
}

// RecordIn is Record within a transaction the caller holds: what a new project starts from, made
// in the same step as the project.
func (s *Service) RecordIn(ctx context.Context, st Store, d Draft, by Author, status string) (db.BrainEntry, error) {
	d, err := s.prepare(d)
	if err != nil {
		return db.BrainEntry{}, err
	}
	held := func(_ context.Context, fn func(Store) error) error { return fn(st) }
	return s.put(ctx, held, d, by, status, by.ID, pgtype.UUID{})
}

// Revise writes a new version of an entry. The old one stays: approving the new one is what
// stops it being true.
func (s *Service) Revise(ctx context.Context, r Reader, id pgtype.UUID, c Change) (db.BrainEntry, error) {
	e, rights, err := s.readable(ctx, r, id)
	if err != nil {
		return db.BrainEntry{}, err
	}
	if ReadOnly(SourceOf(e)) {
		return db.BrainEntry{}, invalid("an entry from a pack changes in its pack")
	}
	if !rights.Propose && !rights.Approve {
		return db.BrainEntry{}, ErrForbidden
	}
	d := draftOf(e)
	if c.Title != nil {
		d.Title = *c.Title
	}
	if c.Content != nil {
		d.Content = *c.Content
	}
	if c.Visibility != nil {
		d.Visibility = *c.Visibility
	}
	if d, err = s.prepare(d); err != nil {
		return db.BrainEntry{}, err
	}
	// The new version is asked about as it will be: shared somewhere else, it needs the rights
	// there too.
	next, err := s.policy.Rights(ctx, r, d.probe())
	if err != nil {
		return db.BrainEntry{}, err
	}
	if !next.Propose && !next.Approve {
		return db.BrainEntry{}, ErrForbidden
	}
	rights.Approve = rights.Approve && next.Approve
	status := StatusProposed
	if rights.Approve && !c.Propose && !r.IsJob() {
		status = StatusApproved
	}
	by := Author{Kind: ByUser, ID: r.User}
	if r.IsJob() {
		by = Author{Kind: ByRunner, ID: r.Job}
	}
	return s.put(ctx, s.tx, d, by, status, r.User, e.Lineage)
}

// Decide approves, rejects or archives an entry, as r.
func (s *Service) Decide(ctx context.Context, r Reader, id pgtype.UUID, verdict string) (db.BrainEntry, error) {
	e, rights, err := s.readable(ctx, r, id)
	if err != nil {
		return db.BrainEntry{}, err
	}
	if !rights.Approve {
		return db.BrainEntry{}, ErrForbidden
	}
	return s.decide(ctx, e, verdict, r.User)
}

func (s *Service) decide(ctx context.Context, e db.BrainEntry, verdict string, by pgtype.UUID) (db.BrainEntry, error) {
	status, ok := map[string]string{VerdictApprove: StatusApproved, VerdictReject: StatusRejected, VerdictArchive: StatusArchived}[verdict]
	if !ok {
		return db.BrainEntry{}, invalid("a verdict is approve, reject or archive")
	}
	if verdict == VerdictReject && e.Status != StatusProposed {
		return db.BrainEntry{}, invalid("only a proposal is rejected; archive an entry instead")
	}
	if e.Status == StatusSuperseded && verdict == VerdictApprove {
		return db.BrainEntry{}, invalid("a newer version replaced this one; revise that instead")
	}
	var out db.BrainEntry
	err := s.tx(ctx, func(st Store) error {
		if err := st.LockLineage(ctx, e.Lineage); err != nil {
			return err
		}
		if status == StatusApproved {
			newest, err := st.NewestApprovedVersion(ctx, e.Lineage)
			if err != nil {
				return err
			}
			if newest > e.Version {
				return invalid("version %d is approved already; revise that one rather than approve an older one", newest)
			}
		}
		var err error
		out, err = st.SetBrainStatus(ctx, db.SetBrainStatusParams{ID: e.ID, Status: status, ApprovedBy: by})
		if err != nil {
			return err
		}
		if status == StatusApproved {
			if err := run(ctx, st, out, s.hooks.Approved); err != nil {
				return err
			}
		}
		return emit(ctx, st, "brain."+status, out)
	})
	return out, conflict(err)
}

// Detail is one entry as a person reads it: every version, what it links to, where it was used.
type Detail struct {
	Entry     db.BrainEntry
	Versions  []db.BrainEntry
	Links     []db.BrainLink
	Citations []db.ListBrainCitationsRow
}

// Get answers one entry, for r.
func (s *Service) Get(ctx context.Context, r Reader, id pgtype.UUID) (Detail, error) {
	e, _, err := s.readable(ctx, r, id)
	if err != nil {
		return Detail{}, err
	}
	versions, err := s.store.ListBrainLineage(ctx, e.Lineage)
	if err != nil {
		return Detail{}, err
	}
	ids := make([]pgtype.UUID, 0, len(versions))
	for _, v := range versions {
		ids = append(ids, v.ID)
	}
	links, err := s.store.ListBrainLinks(ctx, []pgtype.UUID{e.ID})
	if err != nil {
		return Detail{}, err
	}
	if links, err = s.targets.visibleLinks(ctx, s.store, s.policy, r, e, links); err != nil {
		return Detail{}, err
	}
	cites, err := s.store.ListBrainCitations(ctx, ids)
	if err != nil {
		return Detail{}, err
	}
	// A workspace entry is used in every project; the reader is told of the uses in theirs.
	used := cites[:0]
	for _, c := range cites {
		if r.ReadsProject(c.ProjectID) {
			used = append(used, c)
		}
	}
	return Detail{Entry: e, Versions: versions, Links: links, Citations: used}, nil
}

// Proposals are what waits for r's verdict or attention, in project (or everywhere r reads).
func (s *Service) Proposals(ctx context.Context, r Reader, project pgtype.UUID) ([]db.BrainEntry, error) {
	rows, err := s.store.ListBrainProposals(ctx, project)
	if err != nil {
		return nil, err
	}
	return s.Visible(ctx, r, rows)
}

// ToReview are the approved entries in project (or everywhere r reads) that no job has cited
// since before: the ones worth asking whether they still hold.
func (s *Service) ToReview(ctx context.Context, r Reader, project pgtype.UUID, before time.Time) ([]db.BrainEntry, error) {
	rows, err := s.store.StaleBrainEntries(ctx, db.StaleBrainEntriesParams{ProjectID: project,
		Before: pgtype.Timestamptz{Time: before, Valid: true}})
	if err != nil {
		return nil, err
	}
	return s.Visible(ctx, r, rows)
}

// Mine is everything the Brain holds about r, every version: what "me" is, to read and to take
// away.
func (s *Service) Mine(ctx context.Context, r Reader) ([]db.BrainEntry, error) {
	if !r.User.Valid || r.IsJob() {
		return nil, ErrForbidden
	}
	return s.store.ListUserBrain(ctx, r.User)
}

// Forget deletes everything the Brain holds about r, and answers how many entries that was. What a
// job was handed of it is kept in no record (see the runner's handContext), so nothing is left.
func (s *Service) Forget(ctx context.Context, r Reader) (int64, error) {
	if !r.User.Valid || r.IsJob() {
		return 0, ErrForbidden
	}
	return s.store.DeleteUserBrain(ctx, r.User)
}

// Visible keeps the entries r may read, in order.
func (s *Service) Visible(ctx context.Context, r Reader, rows []db.BrainEntry) ([]db.BrainEntry, error) {
	return visible(ctx, s.policy, r, rows)
}

// visible keeps the entries r may read under policy, in order.
func visible(ctx context.Context, policy Policy, r Reader, rows []db.BrainEntry) ([]db.BrainEntry, error) {
	out := make([]db.BrainEntry, 0, len(rows))
	for _, e := range rows {
		ok, err := policy.CanRead(ctx, r, e)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, e)
		}
	}
	return out, nil
}

// readable loads id and what r may do with it; an entry r may not read is not found.
func (s *Service) readable(ctx context.Context, r Reader, id pgtype.UUID) (db.BrainEntry, Rights, error) {
	e, err := s.store.GetBrainEntry(ctx, id)
	if err != nil {
		return db.BrainEntry{}, Rights{}, notFound(err)
	}
	rights, err := s.policy.Rights(ctx, r, e)
	if err != nil {
		return db.BrainEntry{}, Rights{}, err
	}
	if !rights.Read {
		return db.BrainEntry{}, Rights{}, ErrNotFound
	}
	return e, rights, nil
}

// put stores d as the next version of its lineage — the one given, or the entry with the same
// title in the same place — and runs what approval does when it is approved.
func (s *Service) put(ctx context.Context, tx Tx, d Draft, by Author, status string, approver, lineage pgtype.UUID) (db.BrainEntry, error) {
	source, _ := json.Marshal(d.Source)
	params := db.InsertBrainEntryParams{
		Scope: d.Scope, ProjectID: d.Project, ComponentKey: optional(d.Component), UserID: d.User, AgentID: d.Agent,
		Kind: d.Kind, Topic: optional(d.Topic), Title: d.Title, Content: d.Content, Visibility: d.Visibility,
		Status: status, Version: 1, Confidence: d.Confidence, Source: source, SourceTaskID: d.Task,
		SourceConversationID: d.Conversation, SourcePrivate: d.Private, Position: d.Position,
		CondensedFrom: d.CondensedFrom, CreatedByKind: by.Kind, CreatedBy: by.ID,
	}
	if params.CondensedFrom == nil {
		params.CondensedFrom = []pgtype.UUID{}
	}
	if status == StatusApproved {
		params.ApprovedBy = approver
		params.ApprovedAt = pgtype.Timestamptz{Time: now(), Valid: true}
	}
	var out db.BrainEntry
	err := tx(ctx, func(st Store) error {
		// Whoever writes, a link stays inside the entry's own project.
		if err := s.targets.check(ctx, st, s.policy, nil, d.probe(), d.Links); err != nil {
			return err
		}
		prev, found, err := s.previous(ctx, st, d, lineage)
		if err != nil {
			return err
		}
		if found {
			params.Lineage, params.Version, params.Supersedes = prev.Lineage, prev.Version+1, prev.ID
		}
		if out, err = st.InsertBrainEntry(ctx, params); err != nil {
			return err
		}
		if err := st.NarrowLineageVisibility(ctx, db.NarrowLineageVisibilityParams{Lineage: out.Lineage, Visibility: out.Visibility}); err != nil {
			return err
		}
		for _, l := range d.Links {
			if err := st.InsertBrainLink(ctx, db.InsertBrainLinkParams{EntryID: out.ID, TargetKind: l.TargetKind,
				TargetID: l.TargetID, Relation: l.Relation}); err != nil {
				return err
			}
		}
		effects := map[string][]Effect{StatusProposed: s.hooks.Proposed, StatusApproved: s.hooks.Approved}[status]
		if err := run(ctx, st, out, effects); err != nil {
			return err
		}
		return emit(ctx, st, "brain."+status, out)
	})
	return out, conflict(err)
}

// previous is the version d follows, if any.
func (s *Service) previous(ctx context.Context, st Store, d Draft, lineage pgtype.UUID) (db.BrainEntry, bool, error) {
	var prev db.BrainEntry
	if !lineage.Valid {
		if d.Topic != "" {
			return prev, false, nil // a convention is not versioned by its title
		}
		byTitle, err := st.LatestBrainVersionByTitle(ctx, db.LatestBrainVersionByTitleParams{Scope: d.Scope, ProjectID: d.Project,
			ComponentKey: optional(d.Component), UserID: d.User, AgentID: d.Agent, Kind: d.Kind, Title: d.Title})
		if errors.Is(err, pgx.ErrNoRows) {
			return prev, false, nil // two writers of a first version meet at the unique title index
		}
		if err != nil {
			return prev, false, err
		}
		lineage = byTitle.Lineage
	}
	// The lineage is this writer's until it commits; the newest version is read under that lock.
	if err := st.LockLineage(ctx, lineage); err != nil {
		return prev, false, err
	}
	prev, err := st.LatestBrainVersion(ctx, lineage)
	if errors.Is(err, pgx.ErrNoRows) {
		return prev, false, nil
	}
	return prev, err == nil, err
}

func run(ctx context.Context, st Store, e db.BrainEntry, effects []Effect) error {
	for _, effect := range effects {
		if err := effect(ctx, st, e); err != nil {
			return err
		}
	}
	return nil
}

// supersedeEarlier: an approved version is the true one from now on.
func supersedeEarlier(ctx context.Context, st Store, e db.BrainEntry) error {
	_, err := st.SupersedeOtherVersions(ctx, db.SupersedeOtherVersionsParams{Lineage: e.Lineage, Version: e.Version})
	return err
}

// archiveCondensed: a condensed entry replaces what it was made from, which stays readable.
func archiveCondensed(ctx context.Context, st Store, e db.BrainEntry) error {
	if len(e.CondensedFrom) == 0 {
		return nil
	}
	_, err := st.ArchiveBrainEntries(ctx, e.CondensedFrom)
	return err
}

// emit writes what happened to an entry into the outbox: ids, never words, so that what was said
// in a private conversation does not travel. A person's own context makes no event at all.
func emit(ctx context.Context, st Store, typ string, e db.BrainEntry) error {
	if e.Scope == ScopeUser {
		return nil
	}
	fields := map[string]any{"entry_id": UUIDString(e.ID), "kind": e.Kind, "scope": e.Scope, "status": e.Status}
	// Where a proposal from a private conversation was made is that conversation's to know: the
	// project's people read the event, not the conversation.
	if !e.SourcePrivate {
		fields["task_id"], fields["conversation_id"] = UUIDString(e.SourceTaskID), UUIDString(e.SourceConversationID)
	}
	payload, _ := json.Marshal(fields)
	aggregate, id := "project", e.ProjectID
	if !id.Valid {
		aggregate, id = "brain", e.ID
	}
	_, err := st.InsertEvent(ctx, db.InsertEventParams{Type: typ, Aggregate: aggregate, AggregateID: id, Payload: payload})
	return err
}

// prepare checks and tidies a draft before it is stored.
func (s *Service) prepare(d Draft) (Draft, error) {
	d.Scope, d.Kind, d.Topic = strings.TrimSpace(d.Scope), strings.TrimSpace(d.Kind), strings.TrimSpace(d.Topic)
	d.Title, d.Content = strings.TrimSpace(d.Title), strings.TrimSpace(d.Content)
	d.Component = strings.TrimSpace(d.Component)
	switch {
	case !validScope(d.Scope):
		return d, invalid("scope is one of %s", strings.Join(Scopes, ", "))
	case !validKind(d.Kind):
		return d, invalid("kind is one of %s", strings.Join(Kinds, ", "))
	case d.Title == "" || d.Content == "":
		return d, invalid("an entry needs a title and content")
	case len(d.Title) > maxTitle:
		return d, invalid("a title is at most %d bytes", maxTitle)
	case len(d.Content) > maxContent:
		return d, invalid("an entry is at most %d bytes; split it", maxContent)
	case (d.Scope == ScopeUser) != slices.Contains(UserKinds, d.Kind):
		return d, invalid("expertise, focus and preferences are a person's (scope user), and only they are")
	case d.Topic != "" && (d.Kind != KindConvention || !slices.Contains(Topics, d.Topic)):
		return d, invalid("a topic (%s) is for conventions", strings.Join(Topics, ", "))
	}
	switch d.Scope {
	case ScopeWorkspace:
		d.Project = pgtype.UUID{}
	case ScopeProject, ScopeComponent, ScopeAgent:
		if !d.Project.Valid {
			return d, invalid("a %s entry belongs to a project", d.Scope)
		}
	}
	if d.Scope == ScopeComponent && d.Component == "" {
		return d, invalid("a component entry names its component")
	}
	if d.Scope != ScopeComponent {
		d.Component = ""
	}
	if d.Scope == ScopeUser && !d.User.Valid {
		return d, invalid("a person's entry names the person")
	}
	if d.Scope == ScopeAgent && !d.Agent.Valid {
		return d, invalid("an agent's entry names the agent")
	}
	if d.Visibility == "" {
		d.Visibility = VisibilityScope
	}
	switch d.Visibility {
	case VisibilityScope, VisibilityWorkspace:
	case VisibilityPrivate:
		if d.Scope != ScopeUser {
			return d, invalid("only a person's entry is private")
		}
	case VisibilityProject:
		if !d.Project.Valid {
			return d, invalid("an entry shared with a project names the project")
		}
	default:
		return d, invalid("visibility is private, scope, project or workspace")
	}
	if d.Confidence == "" {
		d.Confidence = ConfidenceStated
	}
	if d.Position == 0 {
		d.Position = 100
	}
	src, err := s.sources.Check(d.Source)
	if err != nil {
		return d, err
	}
	if d.Private {
		src.Excerpt = "" // what was said in a private conversation is not quoted where others read
	}
	d.Source = src
	for i, l := range d.Links {
		if l.Relation == "" {
			d.Links[i].Relation = "relates"
		}
	}
	return d, nil
}

// probe is the entry d would be, for the rules to look at before it exists: as if approved, since
// what is asked is whether its scope lets the writer write there (whoever writes from a private
// conversation is in it).
func (d Draft) probe() db.BrainEntry {
	return db.BrainEntry{Scope: d.Scope, ProjectID: d.Project, ComponentKey: optional(d.Component), UserID: d.User,
		AgentID: d.Agent, Kind: d.Kind, Visibility: d.Visibility, Status: StatusApproved, SourcePrivate: d.Private,
		SourceConversationID: d.Conversation, ApprovedAt: pgtype.Timestamptz{Valid: true}}
}

// draftOf is e as a draft of its next version.
func draftOf(e db.BrainEntry) Draft {
	d := Draft{Scope: e.Scope, Kind: e.Kind, Title: e.Title, Content: e.Content, Visibility: e.Visibility,
		Confidence: e.Confidence, Project: e.ProjectID, User: e.UserID, Agent: e.AgentID, Source: SourceOf(e),
		Task: e.SourceTaskID, Conversation: e.SourceConversationID, Private: e.SourcePrivate, Position: e.Position}
	if e.ComponentKey != nil {
		d.Component = *e.ComponentKey
	}
	if e.Topic != nil {
		d.Topic = *e.Topic
	}
	return d
}

const (
	maxTitle   = 300
	maxContent = 64 << 10
)

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// conflict is err as a writer is told it: two writers of the same entry at once (a version taken,
// a lock waited on in a circle) is ErrConflict, to try again.
func conflict(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && (pg.Code == "23505" || pg.Code == "40P01" || pg.Code == "40001") {
		return ErrConflict
	}
	return err
}
