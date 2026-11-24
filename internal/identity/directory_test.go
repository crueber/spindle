// directory_test.go — the instance directory and its deactivate/reactivate
// decision (docs/PLAN-ADMIN.md §3.7).
//
// The state lives on users/<username>/user.json (the credential binding);
// deactivation writes the flag plus a revocation watermark, reactivation clears
// the flag and keeps the watermark, and each transition writes exactly one
// audit record. The listing is bounded: a page walks names until it fills, a
// search examines at most DirectoryScanBound accounts and says so.

package identity

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// directoryService is testService with a pinned clock and the audit recorder
// audit_test.go defines (auditService).
func directoryService(t *testing.T) (*Service, *[]auditRec) {
	t.Helper()
	s, recs := auditService(t)
	s.Now = func() time.Time { return testClock() }
	return s, recs
}

// seedUser creates a bound OIDC account with a display name.
func seedUser(t *testing.T, s *Service, username, email, display string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.ResolveUsername(ctx, email); err != nil {
		t.Fatalf("seed %s: %v", username, err)
	}
	if display != "" {
		if _, err := s.PutProfile(ctx, username, display, ""); err != nil {
			t.Fatalf("profile %s: %v", username, err)
		}
	}
}

func TestSetUserDisabledWritesStateAndAudits(t *testing.T) {
	s, recs := directoryService(t)
	ctx := WithActor(context.Background(), "root")
	seedUser(t, s, "alice", "alice@example.com", "Alice")

	doc, err := s.SetUserDisabled(ctx, "ALICE", true, "  spam account  ")
	if err != nil {
		t.Fatal(err)
	}
	if !doc.Disabled || doc.DisabledBy != "root" || doc.DisabledAt != testClock().Format(time.RFC3339) {
		t.Fatalf("doc = %+v", doc)
	}
	if doc.Reason != "spam account" {
		t.Fatalf("reason = %q (trimmed)", doc.Reason)
	}
	if doc.TokensRevokedAt != doc.DisabledAt {
		t.Fatalf("watermark = %q, want the deactivation instant %q", doc.TokensRevokedAt, doc.DisabledAt)
	}
	// The state is persistent and readable by the credential boundary.
	st, err := s.UserState(ctx, "alice")
	if err != nil || !st.Disabled || st.DisabledBy != "root" || st.Reason != "spam account" {
		t.Fatalf("UserState = %+v, %v", st, err)
	}
	if st.TokensRevokedAt.IsZero() || !st.TokensRevokedAt.Equal(testClock()) {
		t.Fatalf("watermark = %v, want %v", st.TokensRevokedAt, testClock())
	}
	if len(*recs) != 1 {
		t.Fatalf("audit records = %+v, want 1", *recs)
	}
	got := (*recs)[0]
	if got.actor != "root" || got.action != AuditUserDeactivate || got.target != "user:alice" || got.detail != "spam account" {
		t.Fatalf("audit = %+v", got)
	}
}

func TestSetUserDisabledIsIdempotent(t *testing.T) {
	s, recs := directoryService(t)
	ctx := WithActor(context.Background(), "root")
	seedUser(t, s, "alice", "alice@example.com", "")
	first, err := s.SetUserDisabled(ctx, "alice", true, "spam")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.SetUserDisabled(ctx, "alice", true, "again")
	if err != nil {
		t.Fatal(err)
	}
	if second.Version != first.Version || second.Reason != "spam" {
		t.Fatalf("second deactivate rewrote the doc: %+v (first %+v)", second, first)
	}
	if len(*recs) != 1 {
		t.Fatalf("audit records = %d, want 1 (a double submit records nothing)", len(*recs))
	}
}

func TestReactivateKeepsTokenWatermark(t *testing.T) {
	s, recs := directoryService(t)
	ctx := WithActor(context.Background(), "root")
	seedUser(t, s, "alice", "alice@example.com", "")
	disabled, err := s.SetUserDisabled(ctx, "alice", true, "spam")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := s.SetUserDisabled(ctx, "alice", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Disabled || doc.DisabledBy != "" || doc.DisabledAt != "" || doc.Reason != "" {
		t.Fatalf("reactivate left the disablement fields: %+v", doc)
	}
	if doc.TokensRevokedAt != disabled.TokensRevokedAt || doc.TokensRevokedAt == "" {
		t.Fatalf("reactivate must KEEP the watermark: %+v", doc)
	}
	if len(*recs) != 2 || (*recs)[1].action != AuditUserReactivate || (*recs)[1].target != "user:alice" {
		t.Fatalf("audit = %+v", *recs)
	}
	st, err := s.UserState(ctx, "alice")
	if err != nil || st.Disabled || st.TokensRevokedAt.IsZero() {
		t.Fatalf("UserState after reactivate = %+v, %v", st, err)
	}
}

func TestSetUserDisabledRejects(t *testing.T) {
	s, _ := directoryService(t)
	ctx := context.Background()
	if _, err := s.SetUserDisabled(ctx, "ghost", true, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown account err = %v, want ErrNotFound", err)
	}
	seedUser(t, s, "alice", "alice@example.com", "")
	if _, err := s.SetUserDisabled(ctx, "not a username!", true, "x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid username err = %v, want ErrInvalid", err)
	}
	long := make([]byte, maxDirectoryReason+1)
	for i := range long {
		long[i] = 'x'
	}
	if _, err := s.SetUserDisabled(ctx, "alice", true, string(long)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("long reason err = %v, want ErrInvalid", err)
	}
	// The audit actor defaults to "system" when no principal was attached.
	if doc, err := s.SetUserDisabled(ctx, "alice", true, "x"); err != nil || doc.DisabledBy != "system" {
		t.Fatalf("system actor = %+v, %v", doc, err)
	}
}

func TestUserStateUnboundIsActive(t *testing.T) {
	s, _ := directoryService(t)
	ctx := context.Background()
	for _, name := range []string{"ghost", "not a username!"} {
		st, err := s.UserState(ctx, name)
		if err != nil || st.Disabled || !st.TokensRevokedAt.IsZero() {
			t.Fatalf("UserState(%q) = %+v, %v", name, st, err)
		}
	}
}

func TestListDirectoryUsersPagesInNameOrder(t *testing.T) {
	s, _ := directoryService(t)
	seedUser(t, s, "alice", "alice@example.com", "Alice")
	seedUser(t, s, "bob", "bob@example.com", "Bob")
	seedUser(t, s, "carol", "carol@example.com", "")
	ctx := context.Background()

	page, err := s.ListDirectoryUsers(ctx, DirectoryQuery{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 2 || page.Rows[0].Username != "alice" || page.Rows[1].Username != "bob" {
		t.Fatalf("page 1 = %+v", page.Rows)
	}
	if page.Next != "bob" {
		t.Fatalf("next cursor = %q, want bob", page.Next)
	}
	if page.Rows[0].DisplayName != "Alice" || !page.Rows[0].Bound {
		t.Fatalf("row facts = %+v", page.Rows[0])
	}
	page2, err := s.ListDirectoryUsers(ctx, DirectoryQuery{After: page.Next, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2.Rows) != 1 || page2.Rows[0].Username != "carol" || page2.Next != "" {
		t.Fatalf("page 2 = %+v next=%q", page2.Rows, page2.Next)
	}
	// The email-alias family is never a row (users/by-email/… is an index).
	for _, row := range append(page.Rows, page2.Rows...) {
		if row.Username == "by-email" {
			t.Fatal("the by-email alias family leaked into the directory")
		}
	}
	// A profile-only principal is listed but not deactivatable.
	if _, err := s.PutProfile(ctx, "dave", "Dave", ""); err != nil {
		t.Fatal(err)
	}
	page3, err := s.ListDirectoryUsers(ctx, DirectoryQuery{After: "carol", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(page3.Rows) != 1 || page3.Rows[0].Username != "dave" || page3.Rows[0].Bound {
		t.Fatalf("profile-only row = %+v", page3.Rows)
	}
}

func TestListDirectoryUsersSearch(t *testing.T) {
	s, _ := directoryService(t)
	seedUser(t, s, "alice", "alice@example.com", "Alice")
	seedUser(t, s, "zelda", "zelda@example.com", "Zelda Zhang")
	seedUser(t, s, "bob", "bob@example.com", "Bob")
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		query string
		want  []string
	}{
		{"username prefix", "ali", []string{"alice"}},
		{"username substring", "lic", []string{"alice"}},
		{"display name", "zhang", []string{"zelda"}},
		{"case-insensitive", "ALICE", []string{"alice"}},
		{"miss", "nobody", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page, err := s.ListDirectoryUsers(ctx, DirectoryQuery{Query: tc.query, Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, row := range page.Rows {
				got = append(got, row.Username)
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("query %q matched %v, want %v", tc.query, got, tc.want)
			}
			if page.Bound != DirectoryScanBound {
				t.Fatalf("search bound = %d, want %d (the page must state it)", page.Bound, DirectoryScanBound)
			}
		})
	}
}

// TestListDirectoryUsersScanIsBounded pins the documented bound: a search that
// would have to look past DirectoryScanBound accounts stops, says so, and
// offers the cursor that reaches the rest.
func TestListDirectoryUsersScanIsBounded(t *testing.T) {
	s, _ := directoryService(t)
	ctx := context.Background()
	for i := 0; i < DirectoryScanBound+5; i++ {
		name := fmt.Sprintf("user%03d", i)
		seedUser(t, s, name, name+"@example.com", "")
	}
	last := fmt.Sprintf("user%03d", DirectoryScanBound+4)
	page, err := s.ListDirectoryUsers(ctx, DirectoryQuery{Query: last, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 0 || !page.Truncated || page.Next == "" {
		t.Fatalf("first bounded search = rows %d truncated=%v next=%q (want an honest, continuable bound)",
			len(page.Rows), page.Truncated, page.Next)
	}
	if page.Examined != DirectoryScanBound {
		t.Fatalf("examined = %d, want exactly the bound %d", page.Examined, DirectoryScanBound)
	}
	seen := 0
	for page.Next != "" {
		seen++
		if seen > 3 {
			t.Fatal("the bounded search never reached the last account")
		}
		page, err = s.ListDirectoryUsers(ctx, DirectoryQuery{Query: last, After: page.Next, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Rows) == 1 && page.Rows[0].Username == last {
			return
		}
	}
	t.Fatalf("the bounded search never reached %s", last)
}

func TestListDirectoryOrgsCountsMembers(t *testing.T) {
	s, _ := directoryService(t)
	ctx := context.Background()
	for _, org := range []string{"acme", "beta"} {
		if _, err := s.CreateOrg(ctx, org, org+" team", "", "owner@example.com"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SetMember(ctx, "acme", "bob@example.com", OrgMember); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListDirectoryOrgs(ctx, DirectoryQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 1 || page.Rows[0].Name != "acme" || page.Rows[0].Members != 2 {
		t.Fatalf("org page 1 = %+v", page.Rows)
	}
	if page.Rows[0].DisplayName != "acme team" {
		t.Fatalf("display name = %q", page.Rows[0].DisplayName)
	}
	if page.Next != "acme" {
		t.Fatalf("org cursor = %q", page.Next)
	}
	page2, err := s.ListDirectoryOrgs(ctx, DirectoryQuery{After: page.Next, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2.Rows) != 1 || page2.Rows[0].Name != "beta" || page2.Next != "" {
		t.Fatalf("org page 2 = %+v next=%q", page2.Rows, page2.Next)
	}
}

func TestOrgRolesForAnnotatesRows(t *testing.T) {
	s, _ := directoryService(t)
	ctx := context.Background()
	seedUser(t, s, "alice", "alice@example.com", "Alice")
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateOrg(ctx, "beta", "Beta", "", "carol@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetMember(ctx, "beta", "alice@example.com", OrgMember); err != nil {
		t.Fatal(err)
	}
	roles, truncated, err := s.OrgRolesFor(ctx, []string{"Alice", "bob"})
	if err != nil || truncated {
		t.Fatalf("OrgRolesFor err=%v truncated=%v", err, truncated)
	}
	got := roles["alice"]
	sort.Slice(got, func(i, j int) bool { return got[i].Org < got[j].Org })
	if len(got) != 2 || got[0].Org != "acme" || !got[0].Owner || got[1].Org != "beta" || got[1].Owner {
		t.Fatalf("alice roles = %+v", got)
	}
	if len(roles["bob"]) != 0 {
		t.Fatalf("bob roles = %+v", roles["bob"])
	}
	if out, _, err := s.OrgRolesFor(ctx, nil); err != nil || len(out) != 0 {
		t.Fatalf("empty request = %+v, %v", out, err)
	}
}

// TestRecordedStateIsPlainJSON pins the wire shape: append-only fields on
// user.json, cleared fields omitted (an active account's bytes keep their
// pre-field shape).
func TestRecordedStateIsPlainJSON(t *testing.T) {
	s, _ := directoryService(t)
	ctx := WithActor(context.Background(), "root")
	seedUser(t, s, "alice", "alice@example.com", "")
	before, _, err := store.GetBytes(ctx, s.Store, UserKey("alice"), store.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetUserDisabled(ctx, "alice", true, "abuse"); err != nil {
		t.Fatal(err)
	}
	disabled, _, err := store.GetBytes(ctx, s.Store, UserKey("alice"), store.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"disabled":true`, `"disabled_by":"root"`, `"disabled_at":`, `"reason":"abuse"`, `"tokens_revoked_at":`} {
		if !strings.Contains(string(disabled), key) {
			t.Fatalf("deactivated user.json lacks %s: %s", key, disabled)
		}
	}
	if _, err := s.SetUserDisabled(ctx, "alice", false, ""); err != nil {
		t.Fatal(err)
	}
	reactivated, _, err := store.GetBytes(ctx, s.Store, UserKey("alice"), store.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"disabled"`, `"disabled_by"`, `"disabled_at"`, `"reason"`} {
		if strings.Contains(string(reactivated), key) {
			t.Fatalf("reactivated user.json still carries %s: %s", key, reactivated)
		}
	}
	// The email binding survives every transition (identity is immutable).
	if !strings.Contains(string(reactivated), `"email":"alice@example.com"`) || !strings.Contains(string(before), `"username":"alice"`) {
		t.Fatalf("binding changed: before %s after %s", before, reactivated)
	}
}

// listFailStore fails ListPrefixes for the directory's walks.
type listFailStore struct {
	store.ObjectStore
	err error
}

func (l *listFailStore) ListPrefixes(context.Context, string, func(string) error) error {
	return l.err
}

func TestDirectoryStoreFailures(t *testing.T) {
	s, _ := directoryService(t)
	ctx := context.Background()
	seedUser(t, s, "alice", "alice@example.com", "Alice")
	boom := errors.New("store down")

	broken := testService()
	broken.Store = &listFailStore{ObjectStore: broken.Store, err: boom}
	if _, err := broken.ListDirectoryUsers(ctx, DirectoryQuery{}); !errors.Is(err, boom) {
		t.Fatalf("users list error = %v", err)
	}
	if _, err := broken.ListDirectoryUsers(ctx, DirectoryQuery{Query: "ali"}); !errors.Is(err, boom) {
		t.Fatalf("users search error = %v", err)
	}
	if _, err := broken.ListDirectoryOrgs(ctx, DirectoryQuery{}); !errors.Is(err, boom) {
		t.Fatalf("orgs list error = %v", err)
	}
	if _, _, err := broken.OrgRolesFor(ctx, []string{"alice"}); !errors.Is(err, boom) {
		t.Fatalf("org roles error = %v", err)
	}

	// A read failure on one account surfaces (never a half page).
	if _, err := s.SetUserDisabled(WithActor(ctx, "root"), "alice", true, "x"); err != nil {
		t.Fatal(err)
	}
	real := s.Store
	s.Store = &keyFailStore{ObjectStore: real, failGet: []string{UserKey("alice")}, err: boom}
	if _, err := s.ListDirectoryUsers(ctx, DirectoryQuery{}); !errors.Is(err, boom) {
		t.Fatalf("user read error = %v", err)
	}
	if _, err := s.UserState(ctx, "alice"); !errors.Is(err, boom) {
		t.Fatalf("UserState read error = %v", err)
	}
	s.Store = real
	// A corrupt binding or profile is an error, not a silently empty row.
	raw := []byte("{not json")
	if _, err := store.PutBytes(ctx, s.Store, UserKey("baduser"), raw, store.PutOptions{ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListDirectoryUsers(ctx, DirectoryQuery{Query: "baduser"}); err == nil {
		t.Fatal("a corrupt user.json must surface, not render as active")
	}
	if _, err := store.PutBytes(ctx, s.Store, ProfileKey("badprofile"), raw, store.PutOptions{ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListDirectoryUsers(ctx, DirectoryQuery{Query: "badprofile"}); err == nil {
		t.Fatal("a corrupt profile.json must surface")
	}
	// An org prefix with no org.json still lists (name + roster), never 500s.
	if _, err := store.PutBytes(ctx, s.Store, "orgs/ghost/members.json", []byte(`{"version":1,"members":[],"updated_at":"x"}`), store.PutOptions{ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	page, err := s.ListDirectoryOrgs(ctx, DirectoryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range page.Rows {
		if row.Name == "ghost" && row.DisplayName == "" && row.Members == 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("org without org.json = %+v", page.Rows)
	}
	// A corrupt org.json is an error.
	if _, err := store.PutBytes(ctx, s.Store, OrgKey("acme"), raw, store.PutOptions{ContentType: "application/json"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListDirectoryOrgs(ctx, DirectoryQuery{}); err == nil {
		t.Fatal("a corrupt org.json must surface")
	}
}

// TestDirectorySearchPageFills pins the search page cursor: more matches than
// the page holds → Next is the last RETURNED match (not a scan-bound stop).
func TestDirectorySearchPageFills(t *testing.T) {
	s, _ := directoryService(t)
	ctx := context.Background()
	for _, name := range []string{"zeta", "zulu", "zoe"} {
		seedUser(t, s, name, name+"@example.com", "")
	}
	page, err := s.ListDirectoryUsers(ctx, DirectoryQuery{Query: "z", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) != 2 || page.Next != "zoe" || page.Truncated {
		t.Fatalf("page = rows %d next %q truncated %v", len(page.Rows), page.Next, page.Truncated)
	}
	rest, err := s.ListDirectoryUsers(ctx, DirectoryQuery{Query: "z", After: page.Next, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rest.Rows) != 1 || rest.Rows[0].Username != "zulu" {
		t.Fatalf("rest = %+v", rest.Rows)
	}
}

// TestOrgRolesTruncation pins the organization walk's bound: it stops at
// DirectoryOrgScanBound and says so (the page never implies completeness).
func TestOrgRolesTruncation(t *testing.T) {
	s, _ := directoryService(t)
	ctx := context.Background()
	seedUser(t, s, "alice", "alice@example.com", "Alice")
	for i := 0; i < DirectoryOrgScanBound+2; i++ {
		org := fmt.Sprintf("org%03d", i)
		if _, err := s.CreateOrg(ctx, org, "", "", "alice@example.com"); err != nil {
			t.Fatal(err)
		}
	}
	roles, truncated, err := s.OrgRolesFor(ctx, []string{"alice"})
	if err != nil || !truncated {
		t.Fatalf("OrgRolesFor truncated=%v err=%v", truncated, err)
	}
	if len(roles["alice"]) > DirectoryOrgScanBound {
		t.Fatalf("walk examined %d orgs, bound is %d", len(roles["alice"]), DirectoryOrgScanBound)
	}
}

func TestListDirectoryOrgsMembersError(t *testing.T) {
	s, _ := directoryService(t)
	ctx := context.Background()
	if _, err := s.CreateOrg(ctx, "acme", "Acme", "", "alice@example.com"); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("members down")
	s.Store = &keyFailStore{ObjectStore: s.Store, failGet: []string{MembersKey("acme")}, err: boom}
	if _, err := s.ListDirectoryOrgs(ctx, DirectoryQuery{}); !errors.Is(err, boom) {
		t.Fatalf("members read error = %v", err)
	}
}

// --- the credential-boundary cache (account_cache.go) ------------------------

// TestAccountStateWarmPathIsFree is the budget test (law 6): the gate's read
// costs one exact-key GET cold and NOTHING warm; the TTL expiry re-reads, and
// a local deactivation invalidates.
func TestAccountStateWarmPathIsFree(t *testing.T) {
	cs := &countStore{ObjectStore: store.NewMemory()}
	s := New(cs, config.Defaults())
	s.Now = testClock
	ctx := context.Background()
	seedUser(t, s, "alice", "alice@example.com", "")
	if _, err := s.SetUserDisabled(WithActor(ctx, "root"), "alice", true, "spam"); err != nil {
		t.Fatal(err)
	}
	clock := testClock()
	s.Now = func() time.Time { return clock }

	cs.gets.Store(0)
	st, err := s.UserState(ctx, "alice") // cold: one read
	if err != nil || !st.Disabled {
		t.Fatalf("cold UserState = %+v, %v", st, err)
	}
	if got := cs.gets.Load(); got != 1 {
		t.Fatalf("cold lookup store GETs = %d, want 1", got)
	}
	for i := 0; i < 50; i++ { // warm: no store op at all
		if st, err := s.UserState(ctx, "alice"); err != nil || !st.Disabled {
			t.Fatalf("warm UserState = %+v, %v", st, err)
		}
	}
	if got := cs.gets.Load(); got != 1 {
		t.Fatalf("50 warm lookups cost %d store GETs, want 0 more (total 1)", got-1)
	}
	if hits, _, _, size := s.accounts.stats(); hits < 50 || size != 1 {
		t.Fatalf("cache stats hits=%d size=%d, want >=50 hits and one entry", hits, size)
	}

	// The TTL re-reads: past the window, the next lookup goes to the store
	// again (and a negative/active change made elsewhere is picked up).
	clock = clock.Add(AccountStateCacheTTL + time.Second)
	s.Now = func() time.Time { return clock }
	if _, err := s.UserState(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if got := cs.gets.Load(); got != 2 {
		t.Fatalf("post-TTL store GETs = %d, want 2", got)
	}

	// A local transition invalidates immediately — no TTL wait.
	if _, err := s.SetUserDisabled(WithActor(ctx, "root"), "alice", false, ""); err != nil {
		t.Fatal(err)
	}
	cs.gets.Store(0)
	if st, err := s.UserState(ctx, "alice"); err != nil || st.Disabled {
		t.Fatalf("after local reactivate = %+v, %v", st, err)
	}
	if got := cs.gets.Load(); got != 1 {
		t.Fatalf("post-invalidation GETs = %d, want exactly one (the cache was dropped)", got)
	}
}

// TestAccountStateNegativeEntryIsCached: a principal with no binding (the
// common unbound-username case) is cached too, so it is not a per-request
// store probe.
func TestAccountStateNegativeEntryIsCached(t *testing.T) {
	cs := &countStore{ObjectStore: store.NewMemory()}
	s := New(cs, config.Defaults())
	s.Now = testClock
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if st, err := s.UserState(ctx, "ghost"); err != nil || st.Disabled {
			t.Fatalf("UserState(ghost) = %+v, %v", st, err)
		}
	}
	if got := cs.gets.Load(); got != 1 {
		t.Fatalf("unbound lookups store GETs = %d, want 1 (negative entry cached)", got)
	}
}

// TestAccountStateErrorsAreNeverCached: a store fault must stay a fault on the
// next attempt (fail closed), never a cached "active".
func TestAccountStateErrorsAreNeverCached(t *testing.T) {
	real := store.NewMemory()
	s := New(real, config.Defaults())
	s.Now = testClock
	ctx := context.Background()
	seedUser(t, s, "alice", "alice@example.com", "")
	if _, err := s.SetUserDisabled(WithActor(ctx, "root"), "alice", true, "spam"); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("store down")
	s.Store = &keyFailStore{ObjectStore: real, failGet: []string{UserKey("alice")}, err: boom}
	if _, err := s.UserState(ctx, "alice"); !errors.Is(err, boom) {
		t.Fatalf("faulty lookup = %v, want the store error", err)
	}
	if hits, misses, _, _ := s.accounts.stats(); hits != 0 || misses != 1 {
		t.Fatalf("cache stats after a fault = hits %d misses %d (nothing may be cached)", hits, misses)
	}
	// The store heals: the SAME instance must read again (no fabricated entry
	// was left behind) and answer disabled.
	s.Store = real
	if st, err := s.UserState(ctx, "alice"); err != nil || !st.Disabled {
		t.Fatalf("after recovery = %+v, %v", st, err)
	}
}

// TestAccountCacheLRUBounds pins the cache type itself: bounded size, LRU
// eviction, TTL expiry, explicit invalidation, nil-safe.
func TestAccountCacheLRUBounds(t *testing.T) {
	now := testClock()
	c := newAccountCache(2, time.Minute)
	c.set("a", auth.AccountState{}, now)
	c.set("b", auth.AccountState{Disabled: true}, now)
	if st, ok := c.get("a", now); !ok || st.Disabled { // 'a' becomes most recent
		t.Fatalf("get a = %+v, %v", st, ok)
	}
	c.set("c", auth.AccountState{}, now) // evicts 'b' (least recent)
	if _, ok := c.get("b", now); ok {
		t.Fatal("LRU did not evict the least recently used entry")
	}
	if _, ok := c.get("a", now); !ok {
		t.Fatal("LRU evicted a recently used entry")
	}
	if _, _, evicted, size := c.stats(); evicted != 1 || size != 2 {
		t.Fatalf("stats evicted=%d size=%d", evicted, size)
	}
	// TTL.
	later := now.Add(2 * time.Minute)
	if _, ok := c.get("a", later); ok {
		t.Fatal("an expired entry was served")
	}
	// Invalidate.
	c.set("d", auth.AccountState{}, now)
	c.invalidate("d")
	if _, ok := c.get("d", now); ok {
		t.Fatal("invalidate left the entry")
	}
	// Defaults and nil-safety.
	if d := newAccountCache(0, 0); d.cap != AccountStateCacheCapacity || d.ttl != AccountStateCacheTTL {
		t.Fatalf("defaults = %d/%v", d.cap, d.ttl)
	}
	var nilCache *accountCache
	if _, ok := nilCache.get("a", now); ok {
		t.Fatal("nil cache returned a hit")
	}
	nilCache.set("a", auth.AccountState{}, now)
	nilCache.invalidate("a")
	if h, m, e, sz := nilCache.stats(); h != 0 || m != 0 || e != 0 || sz != 0 {
		t.Fatalf("nil stats = %d/%d/%d/%d", h, m, e, sz)
	}
}
