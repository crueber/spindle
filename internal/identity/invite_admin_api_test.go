package identity

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
)

// TestRepoInviteAdminAPI pins the repo-invitation API's authority rules the
// Settings → Access page mirrors: admin-only create/list/cancel, and an
// acceptance that can only come from the invitee (the actor/inviter and a
// stranger both fail).
func TestRepoInviteAdminAPI(t *testing.T) {
	s := testService()
	seedOrg(t, s)
	seedRepo(t, s, "acme", "repo")
	ha := testHandler(s, alice) // org owner → repo admin
	hb := testHandler(s, bob)   // org member, no repo role

	// Role floor: a non-admin cannot list or create (403), even authenticated.
	if w := doReq(hb, "GET", "/acme/repo/api/invitations", ""); w.Code != http.StatusForbidden {
		t.Fatalf("member GET invites = %d, want 403", w.Code)
	}
	if w := doReq(hb, "POST", "/acme/repo/api/invitations", `{"subject":"dave@example.com","role":"write"}`); w.Code != http.StatusForbidden {
		t.Fatalf("member POST invite = %d, want 403", w.Code)
	}

	w := doReq(ha, "POST", "/acme/repo/api/invitations", `{"subject":"dave@example.com","role":"write"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("POST invite = %d: %s", w.Code, w.Body.String())
	}
	var created struct {
		ID        string `json:"id"`
		AcceptURL string `json:"accept_url"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.ID == "" || created.AcceptURL == "" {
		t.Fatalf("create body = %s (%v)", w.Body.String(), err)
	}
	// The API's accept_url keeps its frozen API spelling (the page renders
	// the human /invitations twin).
	if created.AcceptURL != "/api/v1/invitations/"+created.ID+"?token="+func() string {
		invs, _ := s.ListRepoInvites(reqCtx(), "acme", "repo", 10)
		if len(invs) != 1 {
			t.Fatalf("pending = %+v", invs)
		}
		return invs[0].Token
	}() {
		t.Fatalf("accept_url = %q", created.AcceptURL)
	}

	// Non-invitee accept: the invite is not in their inbox → 409 "no longer
	// pending"; the inviter (the actor) is not the invitee either.
	if w := doReq(testHandler(s, stranger), "POST", "/api/v1/invitations/"+created.ID+"/accept", ""); w.Code != http.StatusConflict {
		t.Fatalf("stranger accept = %d, want 409", w.Code)
	}
	if w := doReq(ha, "POST", "/api/v1/invitations/"+created.ID+"/accept", ""); w.Code != http.StatusConflict {
		t.Fatalf("inviter accept = %d, want 409", w.Code)
	}
	// The invite is still pending after the refused accepts.
	if invs, err := s.ListRepoInvites(reqCtx(), "acme", "repo", 10); err != nil || len(invs) != 1 {
		t.Fatalf("pending after refused accepts = %+v, %v", invs, err)
	}

	// The admin cancel is the page's verb (DeleteRepoInvite) and answers 204;
	// a second cancel is 404.
	if w := doReq(ha, "DELETE", "/acme/repo/api/invitations/"+created.ID, ""); w.Code != http.StatusNoContent {
		t.Fatalf("DELETE invite = %d: %s", w.Code, w.Body.String())
	}
	if w := doReq(ha, "DELETE", "/acme/repo/api/invitations/"+created.ID, ""); w.Code != http.StatusNotFound {
		t.Fatalf("re-DELETE = %d, want 404", w.Code)
	}

	// Accept after the invitee exists in the bucket: role effective, actor
	// unchanged.
	if w := doReq(ha, "POST", "/acme/repo/api/invitations", `{"subject":"dave@example.com","role":"maintain"}`); w.Code != http.StatusCreated {
		t.Fatalf("POST invite 2 = %d", w.Code)
	}
	invs, _ := s.ListRepoInvites(reqCtx(), "acme", "repo", 10)
	if len(invs) != 1 {
		t.Fatalf("pending 2 = %+v", invs)
	}
	hd := testHandler(s, dave)
	if w := doReq(hd, "POST", "/api/v1/invitations/"+invs[0].ID+"/accept", ""); w.Code != http.StatusOK {
		t.Fatalf("invitee accept = %d: %s", w.Code, w.Body.String())
	}
	if role, _ := s.Resolve(reqCtx(), "acme", "repo", auth.Principal{Name: "dave@example.com"}); role != RoleMaintain {
		t.Fatalf("invitee role = %q, want maintain", role)
	}
	if role, _ := s.Resolve(reqCtx(), "acme", "repo", alice); role != RoleAdmin {
		t.Fatalf("actor role = %q (accept must bind the invitee only)", role)
	}
}
