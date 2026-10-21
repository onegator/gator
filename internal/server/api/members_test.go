package api

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/onegator/gator/internal/server/api/gen"
)

func (h *harness) emailOf(userID string) string {
	h.t.Helper()
	var email string
	if err := h.pool.QueryRow(context.Background(), "SELECT email FROM users WHERE id=$1", userID).Scan(&email); err != nil {
		h.t.Fatal(err)
	}
	return email
}

// ownProject has a person who is no workspace admin start a project, which makes them its admin.
func (h *harness) ownProject(name string) (adminID, adminToken string, project gen.Project) {
	h.t.Helper()
	adminID, adminToken = h.login("member")
	h.token = adminToken
	if code := h.do("POST", "/projects", gen.NewProject{Slug: fmt.Sprintf("%s%d", name, time.Now().UnixNano()), Name: name}, &project); code != 201 {
		h.t.Fatalf("create project: %d", code)
	}
	return adminID, adminToken, project
}

func TestProjectAdminInvitesAndRemovesWithoutWorkspaceAdmin(t *testing.T) {
	h := newHarness(t)
	_, admin, project := h.ownProject("Crew")
	base := "/projects/" + project.Id.String()

	memberID, member := h.login("member")
	h.member(project.Id.String(), memberID, "member")
	viewerID, _ := h.login("member")
	h.member(project.Id.String(), viewerID, "viewer")

	// A member sees who is in the project but may not change it.
	h.token = member
	var members []gen.ProjectMember
	if code := h.do("GET", base+"/members", nil, &members); code != 200 || len(members) != 3 {
		t.Fatalf("a member listing members: %d %+v", code, members)
	}
	for name, call := range map[string]func() int{
		"invite": func() int {
			return h.do("POST", base+"/invitations", gen.NewInvitation{Email: "x@example.com", Role: "member"}, nil)
		},
		"list":   func() int { return h.do("GET", base+"/invitations", nil, nil) },
		"remove": func() int { return h.do("DELETE", base+"/members/"+viewerID, nil, nil) },
		"change role": func() int {
			return h.do("PUT", base+"/members", map[string]string{"userId": viewerID, "role": "admin"}, nil)
		},
		"search": func() int { return h.do("GET", base+"/candidates?q=api", nil, nil) },
	} {
		if code := call(); code != 403 {
			t.Errorf("a member may not %s: got %d", name, code)
		}
	}

	// The project's admin, who runs nothing else in the workspace, does all of it.
	h.token = admin
	email := fmt.Sprintf("Invitee-%d@Example.com", time.Now().UnixNano())
	var issued gen.IssuedInvitation
	if code := h.do("POST", base+"/invitations", gen.NewInvitation{Email: " " + email + " ", Role: "member"}, &issued); code != 201 {
		t.Fatalf("invite: %d", code)
	}
	if issued.Invitation.Email != strings.ToLower(email) || issued.Emailed ||
		!strings.HasPrefix(issued.AppLink, "gator://invite/") || !strings.Contains(issued.Link, "/invite/") {
		t.Fatalf("issued invitation: %+v", issued)
	}
	if time.Until(issued.Invitation.ExpiresAt) < 13*24*time.Hour {
		t.Fatalf("expiry: %v", issued.Invitation.ExpiresAt)
	}
	// Inviting the same address again replaces the invitation and its link.
	var again gen.IssuedInvitation
	h.do("POST", base+"/invitations", gen.NewInvitation{Email: email, Role: "viewer"}, &again)
	var open []gen.Invitation
	h.do("GET", base+"/invitations", nil, &open)
	if len(open) != 1 || open[0].Id != again.Invitation.Id || open[0].Role != "viewer" || open[0].InvitedByName == nil {
		t.Fatalf("open invitations after inviting twice: %+v", open)
	}
	if code := h.do("POST", base+"/invitations", gen.NewInvitation{Email: "not an address", Role: "member"}, nil); code != 400 {
		t.Fatalf("a malformed address: %d", code)
	}
	if code := h.do("POST", base+"/invitations", gen.NewInvitation{Email: h.emailOf(memberID), Role: "admin"}, nil); code != 409 {
		t.Fatalf("inviting someone already in: %d", code)
	}

	// Renewing gives a new link; revoking ends it.
	var renewed gen.IssuedInvitation
	if code := h.do("POST", base+"/invitations/"+again.Invitation.Id.String()+"/renew", nil, &renewed); code != 200 || renewed.AppLink == again.AppLink {
		t.Fatalf("renew: %d %+v", code, renewed)
	}
	if code := h.do("DELETE", base+"/invitations/"+again.Invitation.Id.String(), nil, nil); code != 204 {
		t.Fatalf("revoke: %d", code)
	}
	if code := h.do("DELETE", base+"/invitations/"+again.Invitation.Id.String(), nil, nil); code != 404 {
		t.Fatalf("revoking twice: %d", code)
	}
	h.do("GET", base+"/invitations", nil, &open)
	if len(open) != 0 {
		t.Fatalf("revoked invitation still listed: %+v", open)
	}

	// Finding people already in the workspace: only those outside the project, only enough
	// to pick one.
	outsiderID, _ := h.login("member")
	outsider := h.emailOf(outsiderID)
	var found []gen.ProjectCandidate
	if code := h.do("GET", base+"/candidates?q="+outsider[:len(outsider)-4], nil, &found); code != 200 || len(found) != 1 || found[0].UserId.String() != outsiderID {
		t.Fatalf("search for an outsider: %d %+v", code, found)
	}
	if code := h.do("GET", base+"/candidates?q="+h.emailOf(memberID), nil, &found); code != 200 || len(found) != 0 {
		t.Fatalf("members are not candidates: %+v", found)
	}
	if code := h.do("GET", base+"/candidates?q=a", nil, nil); code != 400 {
		t.Fatalf("a one-letter search: %d", code)
	}
	if code := h.do("PUT", base+"/members", map[string]string{"userId": outsiderID, "role": "viewer"}, nil); code != 204 {
		t.Fatalf("adding someone found: %d", code)
	}

	if code := h.do("DELETE", base+"/members/"+viewerID, nil, nil); code != 204 {
		t.Fatalf("removing a viewer: %d", code)
	}
	if code := h.do("DELETE", base+"/members/"+viewerID, nil, nil); code != 404 {
		t.Fatalf("removing them twice: %d", code)
	}

	// Anyone may leave.
	h.token = member
	if code := h.do("DELETE", base+"/members/"+memberID, nil, nil); code != 204 {
		t.Fatalf("a member leaving: %d", code)
	}
	if code := h.do("GET", base, nil, nil); code != 403 {
		t.Fatalf("after leaving: %d", code)
	}
}

func TestAProjectKeepsAnAdmin(t *testing.T) {
	h := newHarness(t)
	adminID, admin, project := h.ownProject("Kept")
	base := "/projects/" + project.Id.String()
	h.token = admin

	if code := h.do("PUT", base+"/members", map[string]string{"userId": adminID, "role": "member"}, nil); code != 409 {
		t.Fatalf("demoting the last admin: %d", code)
	}
	if code := h.do("DELETE", base+"/members/"+adminID, nil, nil); code != 409 {
		t.Fatalf("removing the last admin: %d", code)
	}

	otherID, other := h.login("member")
	if code := h.do("PUT", base+"/members", map[string]string{"userId": otherID, "role": "admin"}, nil); code != 204 {
		t.Fatalf("a second admin: %d", code)
	}
	if code := h.do("PUT", base+"/members", map[string]string{"userId": adminID, "role": "viewer"}, nil); code != 204 {
		t.Fatalf("stepping down once another admin is there: %d", code)
	}
	h.token = other
	if code := h.do("DELETE", base+"/members/"+otherID, nil, nil); code != 409 {
		t.Fatalf("the new last admin leaving: %d", code)
	}
	if code := h.do("PUT", base+"/members", map[string]string{"userId": "00000000-0000-0000-0000-000000000001", "role": "member"}, nil); code != 404 {
		t.Fatalf("someone who does not exist: %d", code)
	}
}

func TestAcceptingAnInvitationTakesTheInvitedAddress(t *testing.T) {
	h := newHarness(t)
	_, admin, project := h.ownProject("Join")
	base := "/projects/" + project.Id.String()

	inviteeID, invitee := h.login("member")
	_, stranger := h.login("member")

	h.token = admin
	var issued gen.IssuedInvitation
	h.do("POST", base+"/invitations", gen.NewInvitation{Email: strings.ToUpper(h.emailOf(inviteeID)), Role: "member"}, &issued)
	token := strings.TrimPrefix(issued.AppLink, "gator://invite/")

	h.token = stranger
	if code := h.do("POST", "/invitations/accept", gen.AcceptInvitation{Token: token}, nil); code != 403 {
		t.Fatalf("someone else accepting: %d", code)
	}
	if code := h.do("POST", "/invitations/accept", gen.AcceptInvitation{Token: "nonsense"}, nil); code != 404 {
		t.Fatalf("an unknown token: %d", code)
	}

	h.token = invitee
	var joined gen.Project
	if code := h.do("POST", "/invitations/accept", gen.AcceptInvitation{Token: token}, &joined); code != 200 || joined.Id != project.Id || joined.Role == nil || *joined.Role != gen.ProjectRoleMember {
		t.Fatalf("the invitee accepting: %d %+v", code, joined)
	}
	// Opening the link twice is not a failure.
	if code := h.do("POST", "/invitations/accept", gen.AcceptInvitation{Token: token}, nil); code != 200 {
		t.Fatalf("accepting again: %d", code)
	}

	h.token = admin
	var open []gen.Invitation
	h.do("GET", base+"/invitations", nil, &open)
	if len(open) != 0 {
		t.Fatalf("an accepted invitation is not pending: %+v", open)
	}
	// A revoked link is worthless.
	laterID, later := h.login("member")
	h.do("POST", base+"/invitations", gen.NewInvitation{Email: h.emailOf(laterID), Role: "viewer"}, &issued)
	h.do("DELETE", base+"/invitations/"+issued.Invitation.Id.String(), nil, nil)
	h.token = later
	if code := h.do("POST", "/invitations/accept", gen.AcceptInvitation{Token: strings.TrimPrefix(issued.AppLink, "gator://invite/")}, nil); code != 404 {
		t.Fatalf("a revoked invitation: %d", code)
	}
}

func TestInvitePageHandsTheLinkToTheApp(t *testing.T) {
	h := newHarness(t)
	res, err := h.srv.Client().Get(h.srv.URL + "/invite/abcdefghijklmnopqrstuvwxyz012345")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	page, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 || !strings.Contains(string(page), `href="gator://invite/abcdefghijklmnopqrstuvwxyz012345"`) || res.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("invite page: %d %s", res.StatusCode, page)
	}
	res2, _ := h.srv.Client().Get(h.srv.URL + "/invite/%22%3E%3Cscript%3E")
	if res2.StatusCode != 404 {
		t.Fatalf("a token that is not one: %d", res2.StatusCode)
	}
	res2.Body.Close()
}
