package identity

import (
	"context"
	"errors"
	"testing"
)

func TestBootstrap(t *testing.T) {
	s := testService()
	ctx := context.Background()
	// No repos: nothing to do.
	c, sk, err := s.Bootstrap(ctx)
	if err != nil || c != 0 || sk != 0 {
		t.Errorf("empty bootstrap = %d/%d, %v", c, sk, err)
	}
	// The owner namespaces the sweep must distinguish: an org, a
	// first-login username (user.json), a profile-only account
	// (profile.json and no user.json), and a name with no account
	// trace at all.
	if _, err := s.ResolveUsername(ctx, "crueber@gmail.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureProfile(ctx, "profileonly"); err != nil {
		t.Fatal(err)
	}
	if !s.userExists(ctx, "profileonly") {
		t.Fatal("profile-only fixture is not a claimed user")
	}
	if meta, err := s.Store.Head(ctx, UserKey("profileonly")); err != nil || meta != nil {
		t.Fatalf("profile-only fixture must have no user.json: %v %v", meta, err)
	}
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "owner@example.com"); err != nil {
		t.Fatal(err)
	}
	// Four repos via stub lister.
	s.Repos = func(ctx context.Context) ([][2]string, error) {
		return [][2]string{
			{"acme", "one"},         // org: empty
			{"crueber", "reg"},      // user.json trace
			{"profileonly", "solo"}, // profile.json trace
			{"bob", "two"},          // no trace: empty
		}, nil
	}
	c, sk, err = s.Bootstrap(ctx)
	if err != nil || c != 4 || sk != 0 {
		t.Errorf("bootstrap = %d/%d, %v", c, sk, err)
	}
	// The materialised docs carry the owner grant exactly for user
	// namespaces — both trace shapes — and nothing for orgs/unknowns.
	wantBindings := map[[2]string][]AccessBinding{
		{"acme", "one"}:         {},
		{"crueber", "reg"}:      {{Subject: "user:crueber", Role: RoleAdmin}},
		{"profileonly", "solo"}: {{Subject: "user:profileonly", Role: RoleAdmin}},
		{"bob", "two"}:          {},
	}
	for rr, want := range wantBindings {
		doc, ver, err := s.GetAccess(ctx, rr[0], rr[1])
		if err != nil || ver == "" {
			t.Fatalf("%s/%s materialized read: %v %q", rr[0], rr[1], err, ver)
		}
		if doc.Version != 1 || doc.Visibility != VisibilityPublic {
			t.Errorf("%s/%s materialized doc: %+v", rr[0], rr[1], doc)
		}
		if len(doc.RoleBindings) != len(want) {
			t.Errorf("%s/%s bindings = %+v, want %+v", rr[0], rr[1], doc.RoleBindings, want)
			continue
		}
		for i := range want {
			if doc.RoleBindings[i] != want[i] {
				t.Errorf("%s/%s binding[%d] = %+v, want %+v", rr[0], rr[1], i, doc.RoleBindings[i], want[i])
			}
		}
	}
	// Second sweep skips.
	c, sk, err = s.Bootstrap(ctx)
	if err != nil || c != 0 || sk != 4 {
		t.Errorf("re-sweep = %d/%d, %v", c, sk, err)
	}
	// A repo touched by an admin edit is skipped, not clobbered.
	cur, curVer, _ := s.GetAccess(ctx, "bob", "two")
	_ = cur
	if _, err := s.PutAccess(ctx, "bob", "two", curVer, VisibilityPrivate, nil); err != nil {
		t.Fatal(err)
	}
	c, sk, err = s.Bootstrap(ctx)
	if err != nil || c != 0 || sk != 4 {
		t.Errorf("post-edit sweep = %d/%d, %v", c, sk, err)
	}
	if doc, _, _ := s.GetAccess(ctx, "bob", "two"); doc.Visibility != VisibilityPrivate {
		t.Errorf("bootstrap must not clobber edits: %+v", doc)
	}
	// Lister error aborts.
	s.Repos = func(ctx context.Context) ([][2]string, error) { return nil, errBoom }
	if _, _, err := s.Bootstrap(ctx); !errors.Is(err, errBoom) {
		t.Errorf("lister error: %v", err)
	}
	// Cancelled context aborts.
	s.Repos = func(ctx context.Context) ([][2]string, error) {
		return [][2]string{{"a", "b"}}, nil
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := s.Bootstrap(cctx); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled sweep: %v", err)
	}
	// bootstrapOne put error surfaces.
	s2 := testService()
	s2.Store = &errStore{ObjectStore: s2.Store, putErr: errBoom}
	s2.Repos = func(ctx context.Context) ([][2]string, error) {
		return [][2]string{{"a", "b"}}, nil
	}
	if _, _, err := s2.Bootstrap(ctx); !errors.Is(err, errBoom) {
		t.Errorf("put error: %v", err)
	}
	// BootstrapRepo: per-repo materialization (the Seam 5 op body).
	created, err := s.BootstrapRepo(ctx, "solo", "repo")
	if err != nil || !created {
		t.Errorf("BootstrapRepo = %v, %v", created, err)
	}
	created, err = s.BootstrapRepo(ctx, "solo", "repo")
	if err != nil || created {
		t.Errorf("BootstrapRepo re-run = %v, %v", created, err)
	}
}

func TestListReposDefault(t *testing.T) {
	s := testService()
	ctx := context.Background()
	mustAccess(t, s, "acme", "one", VisibilityPublic, nil)
	mustAccess(t, s, "acme", "two", VisibilityPublic, nil)
	mustAccess(t, s, "bob", "three", VisibilityPublic, nil)
	repos, err := s.listRepos(ctx)
	if err != nil {
		t.Fatalf("listRepos: %v", err)
	}
	if len(repos) != 3 {
		t.Errorf("listRepos = %v", repos)
	}
	sErr := testService()
	sErr.Store = &errStore{ObjectStore: sErr.Store, listErr: errBoom}
	if _, err := sErr.listRepos(ctx); !errors.Is(err, errBoom) {
		t.Errorf("listRepos error: %v", err)
	}
}
