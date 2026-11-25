package brain

import (
	"context"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store/db"
)

// Reader is who reads the Brain, as the rules see them. It is plain data: the REST API builds one
// for the person calling, the MCP server for its token, and the packer for the job it packs for
// (Readers does all three), and then they all ask the same Policy.
type Reader struct {
	// User is the person reading. For a job it is the person at the start of its turn's chain
	// (agent_turns.on_behalf_of): whoever asked, and nobody else, lends the agent their private
	// context. Unset for a job nobody asked for, such as a phase of the process.
	User pgtype.UUID
	// Admin is a workspace admin: every project is theirs to read, nobody's private context is.
	Admin bool
	// Roles are the reader's roles by project. A job is a member of its own project only.
	Roles map[[16]byte]auth.Role
	// Project is set when an agent reads, from inside a job of that project (or a preview of
	// one): it reads only what is approved and still true. Unset for a person.
	Project pgtype.UUID
	// Job is the job the agent reads from, when there is one, and Agent the job's agent.
	Job   pgtype.UUID
	Agent pgtype.UUID
}

// ReadsProject says whether r may read what is open to project's members.
func (r Reader) ReadsProject(project pgtype.UUID) bool {
	if !project.Valid {
		return false
	}
	return r.Admin || r.Roles[project.Bytes] >= auth.RoleViewer
}

// IsJob says whether an agent reads.
func (r Reader) IsJob() bool { return r.Project.Valid }

// AgentReader is an agent of project reading, lent the private context of onBehalfOf (unset for
// a job nobody asked for); job is the job it reads from, if any.
func AgentReader(project, onBehalfOf, job pgtype.UUID) Reader {
	return Reader{Project: project, User: onBehalfOf, Job: job, Roles: map[[16]byte]auth.Role{project.Bytes: auth.RoleMember}}
}

// role is r's role in project.
func (r Reader) role(project pgtype.UUID) auth.Role {
	if r.Admin && !r.IsJob() {
		return auth.RoleAdmin
	}
	if !project.Valid {
		return auth.RoleNone
	}
	return r.Roles[project.Bytes]
}

// ConversationAccess answers whether a person may read a conversation. conversations.Access, with
// its store, gives one (see ConversationReaders).
type ConversationAccess interface {
	UserCanReadConversation(ctx context.Context, conversation, user pgtype.UUID) (bool, error)
}

// Policy decides who reads, proposes and approves which entries. It is the one place these rules
// live:
//
//   - A workspace entry is read by everyone, and written and approved by workspace admins.
//   - A project, component or agent entry is read by whoever may read the project, and written and
//     approved by its members; visibility workspace opens it to everyone.
//   - A user entry is its owner's. Visibility project opens it to that project's readers, and
//     workspace to everyone. Nobody else — not a workspace admin — reads a private one.
//   - A job reads what its project's members read, only once approved and still true, and the
//     private entries of the one person its turn acts for (on_behalf_of), never of anybody else.
//   - A proposal from a private conversation (a DM, a private channel) is read only by those who
//     may read that conversation, until a person approves it into its scope.
type Policy struct {
	conversations ConversationAccess // nil: no proposal from a private conversation is readable
}

// NewPolicy builds the rules.
func NewPolicy(conversations ConversationAccess) Policy { return Policy{conversations: conversations} }

// Rights is what one reader may do with one entry.
type Rights struct {
	Read bool
	// Propose is writing a proposal, or a new version that waits for approval.
	Propose bool
	// Approve is approving, rejecting and archiving it, and writing it approved straight away.
	Approve bool
}

// CanRead answers whether r may read e.
func (p Policy) CanRead(ctx context.Context, r Reader, e db.BrainEntry) (bool, error) {
	if r.IsJob() && !Live(e) {
		return false, nil // an agent reads what a person approved and is still true
	}
	if !inReach(r, e) {
		return false, nil
	}
	// Once a person approved it into its scope it is its scope's, superseded or not later.
	if e.SourcePrivate && !e.ApprovedAt.Valid {
		return p.readsConversation(ctx, r, e.SourceConversationID)
	}
	return true, nil
}

// Rights answers everything r may do with e. A draft that is not stored yet is asked about the
// same way, as the entry it would be.
func (p Policy) Rights(ctx context.Context, r Reader, e db.BrainEntry) (Rights, error) {
	read, err := p.CanRead(ctx, r, e)
	if err != nil || !read {
		return Rights{}, err
	}
	out := Rights{Read: true}
	if r.IsJob() {
		// An agent proposes about its own project, never as a person and never for the workspace.
		// An agent's own lessons are its own: not another agent's.
		out.Propose = e.ProjectID.Valid && e.ProjectID.Bytes == r.Project.Bytes &&
			(e.Scope == ScopeProject || e.Scope == ScopeComponent || (e.Scope == ScopeAgent && r.Agent.Valid && e.AgentID == r.Agent))
		return out, nil
	}
	switch e.Scope {
	case ScopeWorkspace:
		out.Propose = r.User.Valid
		out.Approve = r.Admin
	case ScopeProject, ScopeComponent, ScopeAgent:
		role := r.role(e.ProjectID)
		out.Propose = role >= auth.RoleMember
		out.Approve = role >= auth.RoleMember
	case ScopeUser:
		own := r.User.Valid && e.UserID.Bytes == r.User.Bytes
		out.Propose, out.Approve = own, own
		switch e.Visibility {
		case VisibilityProject:
			// Shared only with a project the person is in: their words reach its jobs.
			if !r.ReadsProject(e.ProjectID) {
				out.Propose, out.Approve = false, false
			}
		case VisibilityWorkspace:
			// Shared with everyone, it reaches every project's people: a workspace admin says so.
			out.Approve = r.Admin
		}
	}
	return out, nil
}

// inReach is the scope and visibility part of the rules; Reach is the same in SQL, made wider.
func inReach(r Reader, e db.BrainEntry) bool {
	switch e.Scope {
	case ScopeWorkspace:
		return true
	case ScopeProject, ScopeComponent, ScopeAgent:
		return e.Visibility == VisibilityWorkspace || r.ReadsProject(e.ProjectID)
	case ScopeUser:
		if r.User.Valid && e.UserID.Valid && e.UserID.Bytes == r.User.Bytes {
			return true
		}
		switch e.Visibility {
		case VisibilityWorkspace:
			return true
		case VisibilityProject:
			return r.ReadsProject(e.ProjectID)
		}
	}
	return false
}

// readsConversationID is readsConversation for a conversation named by its id as text.
func (p Policy) readsConversationID(ctx context.Context, r Reader, id string) (bool, error) {
	conv, ok := ParseUUID(id)
	if !ok {
		return false, nil
	}
	return p.readsConversation(ctx, r, conv)
}

func (p Policy) readsConversation(ctx context.Context, r Reader, conv pgtype.UUID) (bool, error) {
	if p.conversations == nil || !conv.Valid || !r.User.Valid || r.IsJob() {
		return false, nil
	}
	return p.conversations.UserCanReadConversation(ctx, conv, r.User)
}

// Reach is the part of the rules a query can apply, so that a search reads candidates rather
// than the whole Brain. It is never narrower than the Policy, which still decides every row.
type Reach struct {
	EveryProject bool
	Projects     []pgtype.UUID
	User         pgtype.UUID
	// LiveOnly is a job's: approved and still true.
	LiveOnly bool
}

// ReachOf is r's reach.
func ReachOf(r Reader) Reach {
	out := Reach{EveryProject: r.Admin && !r.IsJob(), User: r.User, LiveOnly: r.IsJob()}
	for b, role := range r.Roles {
		if role >= auth.RoleViewer {
			out.Projects = append(out.Projects, pgtype.UUID{Bytes: b, Valid: true})
		}
	}
	return out
}
