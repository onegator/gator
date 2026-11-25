package brain

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/onegator/gator/internal/server/auth"
	"github.com/onegator/gator/internal/server/store/db"
)

func id(b byte) pgtype.UUID { return pgtype.UUID{Bytes: [16]byte{b}, Valid: true} }

// readsOnly is a conversation only one person reads.
type readsOnly struct{ conv, user pgtype.UUID }

func (r readsOnly) UserCanReadConversation(_ context.Context, conv, user pgtype.UUID) (bool, error) {
	return conv == r.conv && user == r.user, nil
}

func TestPolicyDecidesWhoReadsWhat(t *testing.T) {
	var (
		ada, bob, eve   = id(1), id(2), id(3)
		alpha, beta     = id(10), id(11)
		dm              = id(20)
		jobAlpha        = id(30)
		member          = map[[16]byte]auth.Role{alpha.Bytes: auth.RoleMember}
		viewer          = map[[16]byte]auth.Role{alpha.Bytes: auth.RoleViewer}
		approved        = StatusApproved
		policy          = NewPolicy(readsOnly{conv: dm, user: ada})
		adaReads        = Reader{User: ada, Roles: member}
		bobReads        = Reader{User: bob, Roles: member}
		eveReads        = Reader{User: eve, Roles: map[[16]byte]auth.Role{beta.Bytes: auth.RoleAdmin}}
		admin           = Reader{User: eve, Admin: true}
		viewerReads     = Reader{User: bob, Roles: viewer}
		jobForAda       = Reader{Job: jobAlpha, Project: alpha, User: ada, Roles: member}
		jobForBob       = Reader{Job: jobAlpha, Project: alpha, User: bob, Roles: member}
		jobForNobody    = Reader{Job: jobAlpha, Project: alpha, Roles: member}
		workspaceEntry  = db.BrainEntry{Scope: ScopeWorkspace, Status: approved, Visibility: VisibilityScope}
		projectEntry    = db.BrainEntry{Scope: ScopeProject, ProjectID: alpha, Status: approved, Visibility: VisibilityScope}
		proposal        = db.BrainEntry{Scope: ScopeProject, ProjectID: alpha, Status: StatusProposed, Visibility: VisibilityScope}
		openProject     = db.BrainEntry{Scope: ScopeComponent, ProjectID: beta, Status: approved, Visibility: VisibilityWorkspace}
		adaPrivate      = db.BrainEntry{Scope: ScopeUser, UserID: ada, Kind: KindPreference, Status: approved, Visibility: VisibilityPrivate}
		adaScope        = db.BrainEntry{Scope: ScopeUser, UserID: ada, Kind: KindPreference, Status: approved, Visibility: VisibilityScope}
		adaShared       = db.BrainEntry{Scope: ScopeUser, UserID: ada, ProjectID: alpha, Kind: KindExpertise, Status: approved, Visibility: VisibilityProject}
		fromDM          = db.BrainEntry{Scope: ScopeProject, ProjectID: alpha, Status: StatusProposed, SourcePrivate: true, SourceConversationID: dm}
		fromDMApproved  = db.BrainEntry{Scope: ScopeProject, ProjectID: alpha, Status: approved, SourcePrivate: true, SourceConversationID: dm}
		supersededEntry = db.BrainEntry{Scope: ScopeProject, ProjectID: alpha, Status: StatusSuperseded}
	)
	cases := []struct {
		name   string
		reader Reader
		entry  db.BrainEntry
		want   bool
	}{
		{"everyone reads the workspace", eveReads, workspaceEntry, true},
		{"a member reads the project", adaReads, projectEntry, true},
		{"a viewer reads the project", viewerReads, projectEntry, true},
		{"someone else's project is closed", eveReads, projectEntry, false},
		{"a workspace admin reads every project", admin, projectEntry, true},
		{"an entry open to the workspace is read by all", adaReads, openProject, true},
		{"a person reads their private entry", adaReads, adaPrivate, true},
		{"nobody else reads it", bobReads, adaPrivate, false},
		{"not even a workspace admin", admin, adaPrivate, false},
		{"scope visibility of a user entry is its owner's", bobReads, adaScope, false},
		{"a shared entry is read in its project", bobReads, adaShared, true},
		{"and not outside it", eveReads, adaShared, false},
		{"a turn Ada asked for reads her private entry", jobForAda, adaPrivate, true},
		{"a turn Bob asked for never does", jobForBob, adaPrivate, false},
		{"nor a phase nobody asked for", jobForNobody, adaPrivate, false},
		{"a turn reads what Ada shared with its project", jobForBob, adaShared, true},
		{"a job reads approved project entries", jobForNobody, projectEntry, true},
		{"a job does not read proposals", jobForAda, proposal, false},
		{"a job does not read what stopped being true", jobForAda, supersededEntry, false},
		{"a member reads proposals", bobReads, proposal, true},
		{"a DM's proposal is read by who reads the DM", adaReads, fromDM, true},
		{"and by no other member", bobReads, fromDM, false},
		{"nor by the workspace admin", admin, fromDM, false},
		{"once approved it belongs to its scope", bobReads, fromDMApproved, true},
	}
	for _, c := range cases {
		got, err := policy.CanRead(context.Background(), c.reader, c.entry)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("%s: read = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPolicyDecidesWhoWritesAndApproves(t *testing.T) {
	ada, bob, alpha, beta, job := id(1), id(2), id(10), id(11), id(30)
	policy := NewPolicy(nil)
	member := Reader{User: ada, Roles: map[[16]byte]auth.Role{alpha.Bytes: auth.RoleMember}}
	viewer := Reader{User: bob, Roles: map[[16]byte]auth.Role{alpha.Bytes: auth.RoleViewer}}
	admin := Reader{User: bob, Admin: true}
	agent := Reader{Job: job, Project: alpha, User: ada, Roles: map[[16]byte]auth.Role{alpha.Bytes: auth.RoleMember}}
	project := db.BrainEntry{Scope: ScopeProject, ProjectID: alpha, Status: StatusApproved}
	other := db.BrainEntry{Scope: ScopeProject, ProjectID: beta, Status: StatusApproved}
	workspace := db.BrainEntry{Scope: ScopeWorkspace, Status: StatusApproved}
	adas := db.BrainEntry{Scope: ScopeUser, UserID: ada, Status: StatusApproved, Visibility: VisibilityPrivate}
	cases := []struct {
		name             string
		reader           Reader
		entry            db.BrainEntry
		propose, approve bool
	}{
		{"a member writes and approves in their project", member, project, true, true},
		{"a viewer only reads", viewer, project, false, false},
		{"a member proposes for the workspace", member, workspace, true, false},
		{"a workspace admin approves it", admin, workspace, true, true},
		{"a person keeps their own context", member, adas, true, true},
		{"nobody writes another's", admin, adas, false, false},
		{"an agent proposes in its project", agent, project, true, false},
		{"never for another project", agent, other, false, false},
		{"never for the workspace", agent, workspace, false, false},
		{"never as the person it acts for", agent, adas, false, false},
	}
	for _, c := range cases {
		r, err := policy.Rights(context.Background(), c.reader, c.entry)
		if err != nil {
			t.Fatal(err)
		}
		if r.Propose != c.propose || r.Approve != c.approve {
			t.Errorf("%s: propose=%v approve=%v, want %v %v", c.name, r.Propose, r.Approve, c.propose, c.approve)
		}
	}
}
