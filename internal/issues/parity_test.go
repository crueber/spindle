package issues

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
)

// parity_test.go — §13 pinning and §14 dependencies: floor, CAS/ordering,
// idempotence, unknown targets, cycles, the unblocked signal.

func TestPinIssueFloorAndIdempotence(t *testing.T) {
	roles := newFakeRoles()
	s := testService(roles)
	a := mustCreate(t, s, "acme", "repo", janeP, "a", "")
	b := mustCreate(t, s, "acme", "repo", janeP, "b", "")

	if _, _, err := s.PinIssue(reqCtx(), "acme", "repo", a.Num, bobP, true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read-only pin err = %v, want forbidden", err)
	}
	if _, _, err := s.PinIssue(reqCtx(), "acme", "repo", a.Num, anonP, true); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("anon pin err = %v, want unauthorized", err)
	}
	grantCurator(roles)
	pins, changed, err := s.PinIssue(reqCtx(), "acme", "repo", a.Num, janeP, true)
	if err != nil || !changed || len(pins) != 1 || pins[0] != a.Num {
		t.Fatalf("pin = %v, %v, %v", pins, changed, err)
	}
	// Idempotent: no write, same set.
	pins, changed, err = s.PinIssue(reqCtx(), "acme", "repo", a.Num, janeP, true)
	if err != nil || changed || len(pins) != 1 {
		t.Fatalf("repeat pin = %v, %v, %v", pins, changed, err)
	}
	if _, _, err := s.PinIssue(reqCtx(), "acme", "repo", 999, janeP, true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown pin err = %v", err)
	}
	// Host admin passes the floor.
	if _, _, err := s.PinIssue(reqCtx(), "acme", "repo", b.Num, adminP, true); err != nil {
		t.Fatalf("admin pin err = %v", err)
	}
	pins, changed, err = s.PinIssue(reqCtx(), "acme", "repo", a.Num, janeP, false)
	if err != nil || !changed || len(pins) != 1 || pins[0] != b.Num {
		t.Fatalf("unpin = %v, %v, %v", pins, changed, err)
	}
	// Unpinning an unpinned issue is a no-op.
	if _, changed, _ := s.PinIssue(reqCtx(), "acme", "repo", a.Num, janeP, false); changed {
		t.Fatal("repeat unpin reported a change")
	}
}

func TestPinnedListOrdering(t *testing.T) {
	roles := newFakeRoles()
	grantCurator(roles)
	s := testService(roles)
	var nums []int
	for i := 0; i < 4; i++ {
		th := mustCreate(t, s, "acme", "repo", janeP, "t", "")
		nums = append(nums, th.Num)
	}
	// Pin the two oldest issues; they must lead the number-descending list.
	if _, _, err := s.PinIssue(reqCtx(), "acme", "repo", nums[0], janeP, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PinIssue(reqCtx(), "acme", "repo", nums[2], janeP, true); err != nil {
		t.Fatal(err)
	}
	res, err := s.ListIssues(reqCtx(), "acme", "repo", janeP, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	got := []int{}
	for _, c := range res.Issues {
		got = append(got, c.Num)
	}
	want := []int{nums[2], nums[0], nums[3], nums[1]}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	if len(res.Pinned) != 2 || res.Pinned[0] != nums[2] || res.Pinned[1] != nums[0] {
		t.Fatalf("pinned = %v", res.Pinned)
	}
	// A state filter still pins first inside the filtered window.
	res, err = s.ListIssues(reqCtx(), "acme", "repo", janeP, ListFilter{State: StateOpen})
	if err != nil {
		t.Fatal(err)
	}
	if res.Issues[0].Num != nums[2] || res.Issues[1].Num != nums[0] {
		t.Fatalf("filtered order = %+v", res.Issues)
	}
	// The pinned block survives RepairIndex and compaction.
	if _, err := s.RepairIndex(reqCtx(), "acme", "repo"); err != nil {
		t.Fatal(err)
	}
	ix, _, err := s.loadIndex(reqCtx(), "acme", "repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Pinned) != 2 {
		t.Fatalf("pinned after repair = %v", ix.Pinned)
	}
}

func TestPinCapAndConcurrentWriters(t *testing.T) {
	roles := newFakeRoles()
	grantCurator(roles)
	s := testService(roles)
	var nums []int
	for i := 0; i < 8; i++ {
		th := mustCreate(t, s, "acme", "repo", janeP, "t", "")
		nums = append(nums, th.Num)
	}
	var wg sync.WaitGroup
	errs := make([]error, len(nums))
	for i, n := range nums {
		wg.Add(1)
		go func(i, n int) {
			defer wg.Done()
			_, _, errs[i] = s.PinIssue(reqCtx(), "acme", "repo", n, janeP, true)
		}(i, n)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent pin: %v", err)
		}
	}
	pins, err := s.PinnedIssues(reqCtx(), "acme", "repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != len(nums) {
		t.Fatalf("pins = %v, want %d entries (a lost CAS update)", pins, len(nums))
	}
}

func TestPinHTTP(t *testing.T) {
	roles := newFakeRoles()
	grantCurator(roles)
	s := testService(roles)
	th := mustCreate(t, s, "acme", "repo", janeP, "t", "")
	h := testHandler(s, janeP)

	rec := doReq(h, http.MethodPut, "/acme/repo/api/issues/"+itoa(th.Num)+"/pin", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT pin = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Pinned  []int `json:"pinned"`
		Changed bool  `json:"changed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Pinned) != 1 || !body.Changed {
		t.Fatalf("PUT body = %+v", body)
	}
	rec = doReq(h, http.MethodDelete, "/acme/repo/api/issues/"+itoa(th.Num)+"/pin", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE pin = %d", rec.Code)
	}
	// Read-only principal is refused by the service floor through the handler.
	h = testHandler(s, bobP)
	if rec := doReq(h, http.MethodPut, "/acme/repo/api/issues/"+itoa(th.Num)+"/pin", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("read-only PUT pin = %d", rec.Code)
	}
}

func TestDependenciesAddRemove(t *testing.T) {
	roles := newFakeRoles()
	grantCurator(roles)
	s := testService(roles)
	a := mustCreate(t, s, "acme", "repo", janeP, "a", "")
	b := mustCreate(t, s, "acme", "repo", janeP, "b", "")
	c := mustCreate(t, s, "acme", "repo", janeP, "c", "")

	// a is blocked_by b; b blocks a.
	changed, err := s.AddDependency(reqCtx(), "acme", "repo", a.Num, janeP, DepBlockedBy, b.Num)
	if err != nil || !changed {
		t.Fatalf("add blocked_by = %v, %v", changed, err)
	}
	view, err := s.DependenciesFor(reqCtx(), "acme", "repo", a.Num, janeP)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.BlockedBy) != 1 || view.BlockedBy[0].Num != b.Num || !view.Blocked || view.Unblocked {
		t.Fatalf("view = %+v", view)
	}
	other, err := s.DependenciesFor(reqCtx(), "acme", "repo", b.Num, janeP)
	if err != nil {
		t.Fatal(err)
	}
	if len(other.Blocks) != 1 || other.Blocks[0].Num != a.Num {
		t.Fatalf("reverse view = %+v", other)
	}
	// Idempotent add.
	if changed, err := s.AddDependency(reqCtx(), "acme", "repo", a.Num, janeP, DepBlockedBy, b.Num); err != nil || changed {
		t.Fatalf("repeat add = %v, %v", changed, err)
	}
	// Closing the blocker flips the signal.
	if _, err := s.PatchIssue(reqCtx(), "acme", "repo", b.Num, janeP, IssuePatch{State: strPtr(StateClosed)}); err != nil {
		t.Fatal(err)
	}
	view, err = s.DependenciesFor(reqCtx(), "acme", "repo", a.Num, janeP)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Unblocked || view.Blocked {
		t.Fatalf("after close view = %+v", view)
	}
	// Remove (through the blocks spelling on the other side).
	if changed, err := s.RemoveDependency(reqCtx(), "acme", "repo", b.Num, janeP, DepBlocks, a.Num); err != nil || !changed {
		t.Fatalf("remove = %v, %v", changed, err)
	}
	if changed, _ := s.RemoveDependency(reqCtx(), "acme", "repo", b.Num, janeP, DepBlocks, a.Num); changed {
		t.Fatal("repeat remove reported a change")
	}
	view, err = s.DependenciesFor(reqCtx(), "acme", "repo", a.Num, janeP)
	if err != nil || len(view.BlockedBy) != 0 || !view.Unblocked {
		t.Fatalf("after remove view = %+v, %v", view, err)
	}
	_ = c
}

func TestDependenciesRefusals(t *testing.T) {
	roles := newFakeRoles()
	grantCurator(roles)
	s := testService(roles)
	a := mustCreate(t, s, "acme", "repo", janeP, "a", "")
	b := mustCreate(t, s, "acme", "repo", janeP, "b", "")
	c := mustCreate(t, s, "acme", "repo", janeP, "c", "")

	if _, err := s.AddDependency(reqCtx(), "acme", "repo", a.Num, janeP, DepBlocks, a.Num); !errors.Is(err, ErrInvalid) {
		t.Fatalf("self err = %v", err)
	}
	if _, err := s.AddDependency(reqCtx(), "acme", "repo", a.Num, janeP, DepBlocks, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing target err = %v", err)
	}
	if _, err := s.AddDependency(reqCtx(), "acme", "repo", a.Num, janeP, "sideways", b.Num); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad kind err = %v", err)
	}
	if _, err := s.AddDependency(reqCtx(), "acme", "repo", a.Num, bobP, DepBlocks, b.Num); !errors.Is(err, ErrForbidden) {
		t.Fatalf("floor err = %v", err)
	}
	// a → b → c, then c → a would close a cycle: refused.
	for _, e := range [][2]int{{a.Num, b.Num}, {b.Num, c.Num}} {
		if _, err := s.AddDependency(reqCtx(), "acme", "repo", e[0], janeP, DepBlocks, e[1]); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.AddDependency(reqCtx(), "acme", "repo", c.Num, janeP, DepBlocks, a.Num); !errors.Is(err, ErrConflict) {
		t.Fatalf("cycle err = %v", err)
	}
	// The reverse direction of an existing link is the same refusal, not a
	// duplicate: it is a two-issue cycle.
	if _, err := s.AddDependency(reqCtx(), "acme", "repo", b.Num, janeP, DepBlocks, a.Num); !errors.Is(err, ErrConflict) {
		t.Fatalf("two-cycle err = %v", err)
	}
}

func TestDependenciesConcurrentAdds(t *testing.T) {
	roles := newFakeRoles()
	grantCurator(roles)
	s := testService(roles)
	hub := mustCreate(t, s, "acme", "repo", janeP, "hub", "")
	var targets []int
	for i := 0; i < 6; i++ {
		th := mustCreate(t, s, "acme", "repo", janeP, "t", "")
		targets = append(targets, th.Num)
	}
	var wg sync.WaitGroup
	errs := make([]error, len(targets))
	for i, n := range targets {
		wg.Add(1)
		go func(i, n int) {
			defer wg.Done()
			_, errs[i] = s.AddDependency(reqCtx(), "acme", "repo", hub.Num, janeP, DepBlockedBy, n)
		}(i, n)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent add: %v", err)
		}
	}
	view, err := s.DependenciesFor(reqCtx(), "acme", "repo", hub.Num, janeP)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.BlockedBy) != len(targets) {
		t.Fatalf("blocked_by = %d, want %d (a lost CAS update)", len(view.BlockedBy), len(targets))
	}
}

func TestDependenciesHTTP(t *testing.T) {
	roles := newFakeRoles()
	grantCurator(roles)
	s := testService(roles)
	a := mustCreate(t, s, "acme", "repo", janeP, "a", "")
	b := mustCreate(t, s, "acme", "repo", janeP, "b", "")
	h := testHandler(s, janeP)
	base := "/acme/repo/api/issues/" + itoa(a.Num) + "/dependencies"

	rec := doReq(h, http.MethodPost, base, `{"kind":"blocks","target":`+itoa(b.Num)+`}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST = %d: %s", rec.Code, rec.Body.String())
	}
	var view DependencyView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Blocks) != 1 || view.Blocks[0].Num != b.Num {
		t.Fatalf("POST view = %+v", view)
	}
	// Unknown top-level key is refused (fail closed).
	if rec := doReq(h, http.MethodPost, base, `{"kind":"blocks","target":`+itoa(b.Num)+`,"extra":1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown key = %d", rec.Code)
	}
	if rec := doReq(h, http.MethodGet, base, ""); rec.Code != http.StatusOK {
		t.Fatalf("GET = %d", rec.Code)
	}
	rec = doReq(h, http.MethodDelete, base+"/blocks/"+itoa(b.Num), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doReq(h, http.MethodGet, "/acme/repo/api/issues/999/dependencies", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown GET = %d", rec.Code)
	}
	// The read gate applies to the deps GET.
	h = testHandler(s, anonP)
	if rec := doReq(h, http.MethodGet, base, ""); rec.Code != http.StatusOK {
		// public repo: anonymous read is allowed by the fake role service
		t.Fatalf("anon GET = %d", rec.Code)
	}
	roles.private["acme/repo"] = true
	if rec := doReq(h, http.MethodGet, base, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon private GET = %d", rec.Code)
	}
	_ = auth.Anonymous()
}

// grantCurator gives jane triage on acme/repo (the curation floor).
func grantCurator(f *fakeRoles) { f.grant("acme", "repo", "jane@example.com", "triage") }
