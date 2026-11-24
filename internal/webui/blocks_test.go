// blocks_test.go — the profile block control (PLAN-V2 §M.5, M1): the
// control round-trips for a signed-in non-self viewer, the list is private to
// the blocker, self-block is refused, and an org is never a block target.
package webui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/issues"
	"github.com/crueber/spindle/internal/moderation"
	"github.com/crueber/spindle/internal/pulls"
	"github.com/crueber/spindle/internal/server/auth"
)

type fakeBlockList struct {
	mu    sync.Mutex
	pairs map[string]map[string]bool // blocker → subject → true
}

func newFakeBlockList() *fakeBlockList {
	return &fakeBlockList{pairs: map[string]map[string]bool{}}
}

func (f *fakeBlockList) Blocked(_ context.Context, blocker, subject string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pairs[blocker][subject], nil
}

func (f *fakeBlockList) List(_ context.Context, principal string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.pairs[principal]))
	for s := range f.pairs[principal] {
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeBlockList) Block(_ context.Context, blocker, subject string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pairs[blocker] == nil {
		f.pairs[blocker] = map[string]bool{}
	}
	if f.pairs[blocker][subject] {
		return false, nil
	}
	f.pairs[blocker][subject] = true
	return true, nil
}

func (f *fakeBlockList) Unblock(_ context.Context, blocker, subject string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.pairs[blocker][subject] {
		return false, nil
	}
	delete(f.pairs[blocker], subject)
	return true, nil
}

func TestProfileBlockControlRoundTrip(t *testing.T) {
	env, _ := alicePublicEnv(t)
	blocks := newFakeBlockList()
	deps := PublicDeps{Blocks: blocks}
	bob := newPublicServer(t, env, auth.Principal{Name: "bob"}, deps)

	// The control renders for a signed-in other viewer; the list does not.
	rec := publicGet(bob, "/alice")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec, "Block @alice", `action="/alice/block"`)
	publicWantNotContains(t, rec, "Blocked (", `action="/bob/unblock"`)

	// POST blocks: 303 back with the flash.
	sameOrigin := map[string]string{"Origin": "http://example.com"}
	rec = publicDo(bob, http.MethodPost, "/alice/block", sameOrigin)
	publicWantStatus(t, rec, http.StatusSeeOther)
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "/alice?flash=") {
		t.Fatalf("Location = %q", loc)
	}
	if blocked, _ := blocks.Blocked(context.Background(), "bob", "alice"); !blocked {
		t.Fatal("block did not persist through the seam")
	}
	// The page now offers Unblock.
	rec = publicGet(bob, "/alice")
	publicWantContains(t, rec, "Unblock @alice", `action="/alice/unblock"`)

	// Alice's own page: the private disclosure lists who SHE blocked.
	alice := newPublicServer(t, env, auth.Principal{Name: "alice"}, deps)
	rec = publicGet(alice, "/alice")
	publicWantContains(t, rec, "Blocked (0)", "Nobody blocked.")
	if rec := publicDo(alice, http.MethodPost, "/bob/block", sameOrigin); rec.Code != http.StatusSeeOther {
		t.Fatalf("alice blocks bob = %d", rec.Code)
	}
	rec = publicGet(alice, "/alice")
	publicWantStatus(t, rec, http.StatusOK)
	publicWantContains(t, rec, "Blocked (1)", "@bob", `action="/bob/unblock"`)
	publicWantNotContains(t, rec, "Block @")

	// A third viewer sees neither the list nor anything about it.
	carol := newPublicServer(t, env, auth.Principal{Name: "carol"}, deps)
	rec = publicGet(carol, "/alice")
	publicWantNotContains(t, rec, "Blocked (", "@bob")

	// Round trip: unblock from the self disclosure.
	rec = publicDo(alice, http.MethodPost, "/bob/unblock", sameOrigin)
	publicWantStatus(t, rec, http.StatusSeeOther)
	if blocked, _ := blocks.Blocked(context.Background(), "alice", "bob"); blocked {
		t.Fatal("unblock did not persist")
	}
	rec = publicGet(alice, "/alice")
	publicWantContains(t, rec, "Blocked (0)", "Nobody blocked.")
}

func TestProfileBlockRefusals(t *testing.T) {
	env, _ := alicePublicEnv(t)
	deps := PublicDeps{
		Blocks: newFakeBlockList(),
		Org: func(context.Context, string) (*identity.Org, error) {
			return &identity.Org{Org: "acme"}, nil
		},
	}
	alice := newPublicServer(t, env, auth.Principal{Name: "alice"}, deps)
	sameOrigin := map[string]string{"Origin": "http://example.com"}

	// Self-block is refused.
	if rec := publicDo(alice, http.MethodPost, "/alice/block", sameOrigin); rec.Code != http.StatusBadRequest {
		t.Fatalf("self block = %d", rec.Code)
	}
	// An org is not a block target.
	if rec := publicDo(alice, http.MethodPost, "/acme/block", sameOrigin); rec.Code != http.StatusBadRequest {
		t.Fatalf("org block = %d", rec.Code)
	}
	// Cross-origin refused.
	if rec := publicDo(alice, http.MethodPost, "/bob/block", map[string]string{"Origin": "http://other"}); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin = %d", rec.Code)
	}
	// Anonymous is not allowed to write (login-required redirect).
	anon := newPublicServer(t, env, auth.Anonymous(), deps)
	if rec := publicDo(anon, http.MethodPost, "/bob/block", sameOrigin); rec.Code == http.StatusSeeOther &&
		strings.Contains(rec.Header().Get("Location"), "flash=") {
		t.Fatalf("anonymous block wrote: %q", rec.Header().Get("Location"))
	}
}

func TestProfileBlockControlHiddenWithoutSeam(t *testing.T) {
	env, _ := alicePublicEnv(t)
	bob := newPublicServer(t, env, auth.Principal{Name: "bob"}, PublicDeps{})
	rec := publicGet(bob, "/alice")
	publicWantNotContains(t, rec, "Block @alice", "Blocked (")
}

// TestPageRefusalMatchesTheAPI proves PLAN-V2 §M.6.1's page arm: the page
// forms call the same verbs, so a blocked comment surfaces the SAME 403 and
// the same neutral text the JSON API answers — and the page never says
// "blocked".
func TestPageRefusalMatchesTheAPI(t *testing.T) {
	f := newCollabFixture(t)
	f.principal = auth.Principal{Name: "bob", Write: true}
	d := issueDeps()
	refusal := fmt.Errorf("%w: %s", issues.ErrForbidden, identity.InteractionRefused)
	d.IssueComment = func(context.Context, string, string, int, auth.Principal, string) (*issues.Event, error) {
		return nil, refusal
	}
	d.PullComment = func(context.Context, string, string, int, auth.Principal, string) (*pulls.Event, error) {
		return nil, fmt.Errorf("%w: %s", pulls.ErrForbidden, identity.InteractionRefused)
	}
	f.deps(d)

	for _, target := range []string{"/acme/widgets/issues/3/comments", "/acme/widgets/pull/5/comments"} {
		rec := f.post(target, url.Values{"body": {"hi"}})
		wantStatus(t, rec, http.StatusForbidden)
		wantContains(t, rec, identity.InteractionRefused)
		if strings.Contains(strings.ToLower(bodyText(rec)), "blocked") {
			t.Fatalf("%s leaks the block: %s", target, firstN(bodyText(rec), 400))
		}
	}
}

// errBlockList fails every mutation (the seam's error mapping arm).
type errBlockList struct {
	fakeBlockList
	err error
}

func (e *errBlockList) Block(context.Context, string, string) (bool, error)   { return false, e.err }
func (e *errBlockList) Unblock(context.Context, string, string) (bool, error) { return false, e.err }

func TestProfileBlockErrorMapping(t *testing.T) {
	env, _ := alicePublicEnv(t)
	sameOrigin := map[string]string{"Origin": "http://example.com"}

	// Service validation errors map to their statuses (never a 503).
	bad := &errBlockList{err: fmt.Errorf("%w: unknown subject", moderation.ErrInvalid)}
	bob := newPublicServer(t, env, auth.Principal{Name: "bob"}, PublicDeps{Blocks: bad})
	if rec := publicDo(bob, http.MethodPost, "/alice/block", sameOrigin); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid = %d", rec.Code)
	}
	bad.err = fmt.Errorf("%w: changed concurrently", moderation.ErrConflict)
	if rec := publicDo(bob, http.MethodPost, "/alice/block", sameOrigin); rec.Code != http.StatusConflict {
		t.Fatalf("conflict = %d", rec.Code)
	}
	bad.err = errors.New("store down")
	if rec := publicDo(bob, http.MethodPost, "/alice/block", sameOrigin); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("outage = %d", rec.Code)
	}
	// A repeated action is a notice, not an error page.
	bob2 := newPublicServer(t, env, auth.Principal{Name: "bob"}, PublicDeps{Blocks: newFakeBlockList()})
	if rec := publicDo(bob2, http.MethodPost, "/alice/block", sameOrigin); rec.Code != http.StatusSeeOther {
		t.Fatalf("first block = %d", rec.Code)
	}
	rec := publicDo(bob2, http.MethodPost, "/alice/block", sameOrigin)
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "Already+blocked") {
		t.Fatalf("repeat = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}
