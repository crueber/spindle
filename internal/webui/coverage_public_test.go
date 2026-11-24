package webui

// coverage_public_test.go — the public group's remaining branches: the owner
// listing's measurement/outage paths, the invitation preview fallbacks, and
// the write-principal/origin guards on the inbox forms. The page shape suite
// lives in public_test.go.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/sizecatalog"
	"github.com/crueber/spindle/internal/store/proto"
)

// coveragePublicRegistry is publicFakeRegistry with a failing Repos read (the
// visibility walk must surface an outage instead of an empty owner page).
type coveragePublicRegistry struct {
	*publicFakeRegistry
	reposErr error
}

func (r *coveragePublicRegistry) Repos(ctx context.Context, owner string) ([]string, error) {
	if r.reposErr != nil {
		return nil, r.reposErr
	}
	return r.publicFakeRegistry.Repos(ctx, owner)
}

func TestCoverageOwnerRepositoryOutage(t *testing.T) {
	reg := &coveragePublicRegistry{
		publicFakeRegistry: &publicFakeRegistry{
			owners: []string{"alice"},
			repos:  map[string][]string{"alice": {"hello"}},
		},
		reposErr: errors.New("visibility store down"),
	}
	env := publicEnv(publicCfg("none", true), reg)
	s := newPublicServer(t, env, auth.Principal{Name: "alice"}, PublicDeps{})
	rec := publicGet(s, "/alice/repositories")
	publicWantStatus(t, rec, http.StatusServiceUnavailable)
	publicWantContains(t, rec, "repositories unavailable")
}

func TestCoverageOwnerRowsMeasurements(t *testing.T) {
	env, _ := alicePublicEnv(t)
	active := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	empty := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	activeTS := proto.TimeFromGo(active)
	emptyTS := proto.TimeFromGo(empty)
	if _, err := sizecatalog.WriteCatalog(context.Background(), env.API.Store, func(*proto.RepoCatalog) (*proto.RepoCatalog, error) {
		return &proto.RepoCatalog{Entries: []*proto.RepoCatalogEntry{
			{Repo: "alice/hello", SizeBytes: 128, LastCommitTime: &activeTS},
			{Repo: "alice/notes", SizeBytes: 0, LastCommitTime: &emptyTS},
		}}, nil
	}); err != nil {
		t.Fatalf("write catalog: %v", err)
	}
	s := newPublicServer(t, env, auth.Principal{Name: "alice"}, PublicDeps{})
	rec := publicGet(s, "/alice/repositories")
	publicWantStatus(t, rec, http.StatusOK)
	// The row carries the catalog's last-commit stamp (rendered relative), not
	// a per-row fetch.
	publicWantContains(t, rec, `datetime="2026-11-18T12:00:00Z"`)
}

// TestCoverageFillRowFlagsHonoursCancellation pins the concurrency rule: a
// cancelled request stops launching badge probes (the join still completes).
func TestCoverageFillRowFlagsHonoursCancellation(t *testing.T) {
	env, _ := alicePublicEnv(t)
	s := newPublicServer(t, env, auth.Principal{Name: "alice"}, PublicDeps{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rows := []repoRowVM{{Owner: "alice", Name: "hello", Full: "alice/hello"}}
	s.fillRowFlags(ctx, "alice", rows)
	if rows[0].VisibilityLabel != "" || rows[0].Mirror {
		t.Fatalf("cancelled probe wrote badges: %+v", rows[0])
	}
}

func TestCoverageInvitationPreviewFallbacks(t *testing.T) {
	env, _ := alicePublicEnv(t)
	deps := publicInviteDeps(nil)
	deps.PreviewInvite = func(_ context.Context, _ auth.Principal, id, _ string) (*identity.Invitation, error) {
		// An org invitation with no explicit kind and no repo: the view must
		// derive "org" and fall through to the org scope.
		return &identity.Invitation{ID: id, Org: "acme", Role: "member", InvitedBy: "alice",
			State: "pending", ExpiresAt: "2030-01-01T00:00:00Z"}, nil
	}
	s := newPublicServer(t, env, auth.Principal{Name: "alice"}, deps)
	rec := publicGet(s, "/invitations?id=inv-shared&token=tok")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec, ">org<", "acme")
}

func TestCoverageInviteRowFallsBackToID(t *testing.T) {
	t.Parallel()
	row := inviteRow(identity.InboxEntry{ID: "inv-42", Role: "member"}, publicFixedNow)
	if row.Scope != "inv-42" || row.Kind != "org" || row.ID != "inv-42" {
		t.Fatalf("inviteRow fallback = %+v", row)
	}
	if got := friendlyInviteErr(errors.New("invitation is not pending")); got != "invitation no longer pending" {
		t.Fatalf("friendlyInviteErr(not pending) = %q", got)
	}
}

func TestCoverageInvitationWriteGuards(t *testing.T) {
	env, _ := alicePublicEnv(t)

	// Accept without the binding is a 503, never a silent no-op.
	deps := publicInviteDeps(nil)
	deps.AcceptInvite = nil
	s := newPublicServer(t, env, auth.Principal{Name: "alice"}, deps)
	rec := publicDo(s, http.MethodPost, "/invitations/inv-1/accept", map[string]string{"Origin": "http://example.com"})
	publicWantStatus(t, rec, http.StatusServiceUnavailable)
	publicWantContains(t, rec, "invitations unavailable")

	// Decline without the binding is a 503 too.
	deps = publicInviteDeps(nil)
	deps.CancelInvite = nil
	s = newPublicServer(t, env, auth.Principal{Name: "alice"}, deps)
	rec = publicDo(s, http.MethodPost, "/invitations/inv-1/decline", map[string]string{"Origin": "http://example.com"})
	publicWantStatus(t, rec, http.StatusServiceUnavailable)

	// Anonymous callers are refused before the service on both actions and on
	// the notification state form.
	s = newPublicServer(t, env, auth.Anonymous(), publicInviteDeps(nil))
	for _, target := range []string{"/invitations/inv-1/accept", "/invitations/inv-1/decline", "/notifications/n1/read"} {
		rec = publicDo(s, http.MethodPost, target, map[string]string{"Origin": "http://example.com", "Accept": "text/plain"})
		publicWantStatus(t, rec, http.StatusUnauthorized)
	}

	// An unparseable Origin is refused (the same-origin rule fails closed).
	s = newPublicServer(t, env, auth.Principal{Name: "alice"}, publicInviteDeps(nil))
	rec = publicDo(s, http.MethodPost, "/invitations/inv-1/accept", map[string]string{"Origin": "::bogus", "Accept": "text/plain"})
	publicWantStatus(t, rec, http.StatusForbidden)

	// A failed authenticator surfaces its status before any handler logic.
	s = newPublicServer(t, env, auth.Principal{Name: "alice"}, publicInviteDeps(nil))
	s.auth.Authenticate = func(*http.Request) (auth.Principal, *auth.AuthError) {
		return auth.Principal{}, &auth.AuthError{Kind: auth.ErrForbidden, Why: "session revoked"}
	}
	rec = publicDo(s, http.MethodPost, "/invitations/inv-1/accept", map[string]string{"Accept": "text/plain"})
	publicWantStatus(t, rec, http.StatusForbidden)
	if !strings.Contains(rec.Body.String(), "session revoked") {
		t.Fatalf("auth failure body = %q", rec.Body.String())
	}

	// The deps seam is process-wide; make sure this test leaves it empty.
	BindPublic(PublicDeps{})
}
