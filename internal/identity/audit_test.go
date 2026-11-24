package identity

// audit_test.go — identity's administrative audit records (PLAN-ADMIN §8.6,
// PLAN-V2 §M.4's completeness sweep): access grants, org/team membership
// grants and revokes, and the transfer record. The family itself is
// moderation's; this file proves identity asks for the write, names the right
// action and target, and attributes the actor.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// auditRec is one captured identity audit call.
type auditRec struct {
	actor, action, target, detail string
}

// auditService wires the capture seam.
func auditService(t *testing.T) (*Service, *[]auditRec) {
	t.Helper()
	s := testService()
	var got []auditRec
	s.Audit = func(_ context.Context, actor, action, target, detail string) error {
		got = append(got, auditRec{actor, action, target, detail})
		return nil
	}
	return s, &got
}

func TestPutAccessAuditsGrant(t *testing.T) {
	s, got := auditService(t)
	ctx := WithActor(context.Background(), "alice@example.com")
	if _, err := s.PutAccess(ctx, "acme", "widgets", "", VisibilityPrivate,
		[]AccessBinding{{Subject: "user:bob@example.com", Role: RoleWrite}}); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 {
		t.Fatalf("audits = %v", *got)
	}
	r := (*got)[0]
	if r.action != AuditAccessGrant || r.target != "repo:acme/widgets" || r.actor != "alice@example.com" {
		t.Fatalf("record = %+v", r)
	}
	if !strings.Contains(r.detail, "bindings=1") || !strings.Contains(r.detail, "visibility=private") {
		t.Fatalf("detail = %q", r.detail)
	}
}

func TestOrgMembershipAudits(t *testing.T) {
	s, got := auditService(t)
	ctx := WithActor(context.Background(), "owner@example.com")
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "owner@example.com"); err != nil {
		t.Fatal(err)
	}
	*got = (*got)[:0]
	if _, err := s.SetMember(ctx, "acme", "bob@example.com", OrgMember); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveMember(ctx, "acme", "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 2 {
		t.Fatalf("audits = %v", *got)
	}
	if g := (*got)[0]; g.action != AuditOrgMemberGrant || g.target != "org:acme" ||
		!strings.Contains(g.detail, "principal=bob@example.com") || !strings.Contains(g.detail, "added=yes") {
		t.Fatalf("grant = %+v", g)
	}
	if r := (*got)[1]; r.action != AuditOrgMemberRevoke || r.target != "org:acme" || r.actor != "owner@example.com" {
		t.Fatalf("revoke = %+v", r)
	}
}

func TestTeamMembershipAudits(t *testing.T) {
	s, got := auditService(t)
	ctx := WithActor(context.Background(), "owner@example.com")
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "owner@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTeam(ctx, "acme", "core", "Core", ""); err != nil {
		t.Fatal(err)
	}
	*got = (*got)[:0]
	if _, err := s.SetTeamMember(ctx, "acme", "core", "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveTeamMember(ctx, "acme", "core", "bob@example.com"); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 2 {
		t.Fatalf("audits = %v", *got)
	}
	if g := (*got)[0]; g.action != AuditTeamGrant || g.target != "team:acme/core" {
		t.Fatalf("grant = %+v", g)
	}
	if r := (*got)[1]; r.action != AuditTeamRevoke || r.target != "team:acme/core" {
		t.Fatalf("revoke = %+v", r)
	}
}

// TestAuditActorFallback: a caller that never attached an actor (the CLI) is
// recorded as "system", never as an empty string the viewer cannot read.
func TestAuditActorFallback(t *testing.T) {
	s, got := auditService(t)
	if _, err := s.PutAccess(context.Background(), "acme", "widgets", "", VisibilityPublic, nil); err != nil {
		t.Fatal(err)
	}
	if (*got)[0].actor != "system" {
		t.Fatalf("actor = %q, want system", (*got)[0].actor)
	}
}

// TestAuditFailureDoesNotFailTheWrite: the grant lands even when the family is
// down (audit-after-commit).
func TestAuditFailureDoesNotFailTheWrite(t *testing.T) {
	s := testService()
	s.Audit = func(context.Context, string, string, string, string) error {
		return errors.New("audit family down")
	}
	if _, err := s.PutAccess(context.Background(), "acme", "widgets", "", VisibilityPublic, nil); err != nil {
		t.Fatalf("a failed audit write failed the grant: %v", err)
	}
	// Nil seam = no family on this build: silent, not a panic.
	s2 := testService()
	if _, err := s2.PutAccess(context.Background(), "acme", "widgets", "", VisibilityPublic, nil); err != nil {
		t.Fatal(err)
	}
}

// TestTransferAuditsCompletion: the transfer's record names both sides and is
// written only after the move verified (transferRun's last act).
func TestTransferAuditsCompletion(t *testing.T) {
	s, got := auditService(t)
	seedTransferRepo(t, s, "acme", "r1")
	ctx := WithActor(context.Background(), "alice@example.com")
	if err := s.TransferRepo(ctx, "acme", "r1", "acme", "r2"); err != nil {
		t.Fatalf("transfer = %v", err)
	}
	if len(*got) != 1 {
		t.Fatalf("audits = %v", *got)
	}
	r := (*got)[0]
	if r.action != AuditTransfer || r.target != "repo:acme/r2" || r.actor != "alice@example.com" {
		t.Fatalf("record = %+v", r)
	}
	if !strings.Contains(r.detail, "from=acme/r1") || !strings.Contains(r.detail, "to=acme/r2") {
		t.Fatalf("detail = %q", r.detail)
	}
	// A failed transfer writes nothing (no half-moved claim in the log).
	s2, got2 := auditService(t)
	if err := s2.TransferRepo(context.Background(), "acme", "nope", "acme", "r2"); err == nil {
		t.Fatal("an unknown source must fail")
	}
	if len(*got2) != 0 {
		t.Fatalf("a failed transfer audited: %v", *got2)
	}
}

// TestWithActorOverridesNothing proves the helper is attribution only: it
// carries no principal and changes no gate.
func TestWithActorCarriesOnlyAName(t *testing.T) {
	ctx := WithActor(context.Background(), "alice@example.com")
	if ActorOf(ctx) != "alice@example.com" {
		t.Fatalf("ActorOf = %q", ActorOf(ctx))
	}
	if ActorOf(context.Background()) != "" {
		t.Fatal("an unattached context reported an actor")
	}
	// The audit record reaches the store family through composition in
	// production; here the zero-value check keeps the seam honest.
	s := testService()
	if _, err := store.PutBytes(ctx, s.Store, "keep", []byte("x"), store.PutOptions{Mode: store.PutCreate}); err != nil {
		t.Fatal(err)
	}
}

// TestHandlerAttributesTheActor proves the HTTP lane wires the verified
// principal into the audit record (identity.WithActor), so an access grant from
// the API names its operator instead of "system".
func TestHandlerAttributesTheActor(t *testing.T) {
	s, got := auditService(t)
	seedOrg(t, s) // acme, alice is an owner
	seedRepo(t, s, "acme", "repo")
	*got = (*got)[:0]
	h := testHandler(s, alice) // org owner passes the repo admin floor
	w := doReq(h, "PUT", "/acme/repo/api/access", `{"version":0,"visibility":"private","role_bindings":[]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT access = %d: %s", w.Code, w.Body.String())
	}
	if len(*got) != 1 {
		t.Fatalf("audits = %v", *got)
	}
	if r := (*got)[0]; r.actor != alice.Name || r.action != AuditAccessGrant {
		t.Fatalf("record = %+v", r)
	}
	// The org roster lane attributes too.
	if _, err := s.CreateOrg(WithActor(context.Background(), alice.Name), "beta", "Beta", "", "owner@example.com"); err != nil {
		t.Fatal(err)
	}
	*got = (*got)[:0]
	ho := testHandler(s, auth.Principal{Name: "owner@example.com", Write: true, Admin: true})
	w = doReq(ho, "PUT", "/api/v1/orgs/beta/members/member@example.com", `{"role":"member"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("PUT member = %d: %s", w.Code, w.Body.String())
	}
	if len(*got) != 1 || (*got)[0].actor != "owner@example.com" || (*got)[0].action != AuditOrgMemberGrant {
		t.Fatalf("roster record = %v", *got)
	}
}
