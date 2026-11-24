// directory.go — the instance-wide users and organizations directory and its
// one mutation: deactivate / reactivate an account (docs/PLAN-ADMIN.md §3.7,
// the §8.6 follow-up card that needed this schema decision).
//
// # The decision (law 8)
//
// Deactivation is stored on users/<username>/user.json — the credential
// binding — as append-only fields (disabled, disabled_by, disabled_at,
// reason, tokens_revoked_at; see UserDoc). It is NOT on profile.json:
//
//   - profile.json is presentation state (display name, bio, avatar) that the
//     public profile page reads; a credential fact must not ride a document
//     whose whole job is to be shown around.
//   - user.json is the OIDC registry: every sign-in creates it, and the
//     credential boundary already resolves a username for every request, so
//     enforcement is one exact-key GET of the document the account IS.
//   - it is extensible JSON with a CAS token (Version), so the flag is a
//     plain additive field — no key moves, no migration (law 5).
//
// # Enforcement
//
// internal/server/auth.go's accountGate reads this state after OIDC
// verification and username resolution, for every OIDC-derived credential:
// ID tokens, wgt_ tokens, browser sessions, forwarded identities and SSH key
// auth. Machine credentials (server.auth.tokens, CI/runner tokens) are
// configuration entries and are not user-bound, so they are untouched.
//
// The read is NOT one bucket round trip per request: UserState consults the
// bounded in-process cache in account_cache.go (LRU, 4096 entries, 10 s TTL,
// negative entries included, errors never cached), invalidated here on every
// SetUserDisabled commit. A warm lookup costs no store op; the TTL is the
// cross-instance staleness bound (see that file's header).
//
// # Tokens and sessions
//
// wgt_ tokens and browser sessions are HMAC wires (no server-side registry to
// delete), so revocation is a WATERMARK: deactivation writes
// tokens_revoked_at = now and a session/token whose mint time is at or before
// it is refused, permanently. Reactivation clears the disablement but not the
// watermark — the account must sign in again, and every previously minted
// credential stays dead. A fresh ID token from the IdP is not subject to the
// watermark (it is a new credential from the identity provider), only to the
// disabled flag.
//
// # Bounds (law 4/6)
//
// The directory is an administrator page, not a hot path: the user listing is
// one delimiter LIST streamed until the page (limit+1) or the scan bound is
// reached, and a search examines at most DirectoryScanBound accounts in name
// order (each row costs its user.json + profile.json read). Every page states
// the bound it used. There is no index document to keep in sync and no LIST on
// any request path other than this page's.
package identity

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// Directory list bounds. They are deliberate product numbers, not tuning
// knobs: an administrator page may pay a bounded number of exact-key reads.
const (
	// DirectoryPageDefault is the page size when the request names none.
	DirectoryPageDefault = 25
	// DirectoryPageMax caps a caller-supplied page size.
	DirectoryPageMax = 100
	// DirectoryScanBound caps how many accounts one search request examines.
	// A search walks usernames in name order and stops here, reporting that
	// it did (so the page can say "refine the search" instead of implying a
	// complete answer).
	DirectoryScanBound = 120
	// DirectoryOrgScanBound caps the organization walk behind member counts
	// and the "this account owns organizations" check.
	DirectoryOrgScanBound = 300
	// maxDirectoryReason is the deactivation reason budget (runes).
	maxDirectoryReason = 500
)

// Audit verbs the directory writes (the audit viewer's filter vocabulary).
const (
	AuditUserDeactivate = "user_deactivate"
	AuditUserReactivate = "user_reactivate"
)

// DirectoryQuery is one directory page request.
type DirectoryQuery struct {
	// Query is the case-insensitive substring filter over username and
	// display name ("" lists everything in name order).
	Query string
	// After is the cursor: the last name the previous page returned or
	// examined. Empty starts at the beginning, in name order.
	After string
	// Limit is the requested page size (0 → DirectoryPageDefault).
	Limit int
}

func (q DirectoryQuery) limit() int {
	switch {
	case q.Limit <= 0:
		return DirectoryPageDefault
	case q.Limit > DirectoryPageMax:
		return DirectoryPageMax
	}
	return q.Limit
}

// DirectoryUser is one row of the users directory.
type DirectoryUser struct {
	Username string
	// DisplayName is profile.json's display_name ("" when none is set).
	DisplayName string
	// Bound is false for a principal that has only a profile (a token-mode
	// name, a grant subject): it has no sign-in account, so it cannot be
	// deactivated and the page says so instead of offering the control.
	Bound bool
	// Disabled and its explanation fields mirror user.json.
	Disabled   bool
	DisabledBy string
	DisabledAt string
	Reason     string
	// TokensRevokedAt is the revocation watermark ("" = none).
	TokensRevokedAt string
}

// DirectoryUserPage is one page of the users directory.
type DirectoryUserPage struct {
	Rows []DirectoryUser
	// Next is the cursor for the following page ("" = nothing after it).
	Next string
	// Examined is how many account names this request looked at (the scan
	// bound makes that number meaningful to the reader).
	Examined int
	// Bound is the search bound that applied (0 for an unfiltered listing,
	// which walks names only until the page fills).
	Bound int
	// Truncated reports that the search stopped at Bound with more names
	// unexamined — the page must not imply a complete answer.
	Truncated bool
}

// DirectoryOrg is one row of the organizations directory.
type DirectoryOrg struct {
	Name        string
	DisplayName string
	// Members is the roster size (members.json; 0 when the file is absent).
	Members int
}

// DirectoryOrgPage is one page of the organizations directory.
type DirectoryOrgPage struct {
	Rows    []DirectoryOrg
	Next    string
	Scanned int
	Bound   int
}

// OrgMembership is one organization a principal belongs to, with its roster
// role (owner or member). Named apart from the roster's OrgRole string type.
type OrgMembership struct {
	Org   string
	Owner bool
}

// UserState reads an account's directory state — the credential boundary's
// read, where a missing binding is simply "not disabled".
//
// It is served from the bounded in-process cache (account_cache.go) when a
// live entry exists, so a signed-in request costs NO store op on the warm
// path; a cold or expired entry is one exact-key GET (never a LIST) and is
// then cached — a successful read only, negative (unbound) results included.
// A store error is returned uncached, so the gate fails closed on every
// attempt rather than serving a fabricated "active".
func (s *Service) UserState(ctx context.Context, username string) (auth.AccountState, error) {
	username = normPrincipal(username)
	if !auth.ValidUsername(username) {
		return auth.AccountState{}, nil
	}
	now := s.nowUTC()
	if st, ok := s.accounts.get(username, now); ok {
		return st, nil
	}
	raw, _, err := store.GetBytes(ctx, s.Store, UserKey(username), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			st := auth.AccountState{}
			s.accounts.set(username, st, now)
			return st, nil
		}
		return auth.AccountState{}, err // never cached: a fault is a 503 every time
	}
	u, perr := parseUserDoc(raw)
	if perr != nil {
		return auth.AccountState{}, perr
	}
	st := accountStateOf(u)
	s.accounts.set(username, st, now)
	return st, nil
}

// accountStateOf projects a user doc onto the credential gate's state.
func accountStateOf(u *UserDoc) auth.AccountState {
	st := auth.AccountState{
		Disabled:   u.Disabled,
		DisabledBy: u.DisabledBy,
		DisabledAt: u.DisabledAt,
		Reason:     u.Reason,
	}
	if u.TokensRevokedAt != "" {
		if t, err := time.Parse(time.RFC3339, u.TokensRevokedAt); err == nil {
			st.TokensRevokedAt = t
		}
	}
	return st
}

// SetUserDisabled deactivates (disabled=true) or reactivates (false) an OIDC
// account and audits the transition. It is CAS'd on user.json (the only commit
// point) and idempotent: a call that does not change the state writes nothing
// and records nothing, so a double submit cannot fabricate an audit entry.
//
// Deactivate sets disabled_by from the request context's actor (WithActor),
// disabled_at/reason, and the tokens_revoked_at watermark. Reactivate clears
// the disablement fields and KEEPS the watermark — revoked credentials are
// never restored.
//
// It never touches org membership or ownership: a disabled owner still owns
// their organizations (the page says so; transfer is a separate operation).
func (s *Service) SetUserDisabled(ctx context.Context, username string, disabled bool, reason string) (*UserDoc, error) {
	username = normPrincipal(username)
	if !auth.ValidUsername(username) {
		return nil, fmt.Errorf("%w: invalid username %q", ErrInvalid, username)
	}
	reason = strings.TrimSpace(reason)
	if len([]rune(reason)) > maxDirectoryReason {
		return nil, fmt.Errorf("%w: reason too long (max %d characters)", ErrInvalid, maxDirectoryReason)
	}
	who := normPrincipal(ActorOf(ctx))
	if who == "" {
		who = "system"
	}
	now := s.nowUTC().Format(time.RFC3339)
	changed := false
	var result *UserDoc
	_, err := s.casUpdate(ctx, UserKey(username), func(cur []byte, _ store.Version) ([]byte, bool, error) {
		if cur == nil {
			return nil, false, fmt.Errorf("%w: no sign-in account for %q", ErrNotFound, username)
		}
		u, perr := parseUserDoc(cur)
		if perr != nil {
			return nil, false, perr
		}
		if u.Disabled == disabled {
			result = u // already in the requested state: nothing to write
			return nil, false, nil
		}
		u.Version++
		if disabled {
			u.Disabled = true
			u.DisabledBy = who
			u.DisabledAt = now
			u.Reason = reason
			u.TokensRevokedAt = now
		} else {
			u.Disabled = false
			u.DisabledBy = ""
			u.DisabledAt = ""
			u.Reason = ""
			// TokensRevokedAt deliberately survives reactivation.
		}
		changed = true
		result = u
		return encodeUserDoc(u), true, nil
	})
	if err != nil {
		return nil, err
	}
	// This instance enforces its own write immediately: the credential
	// boundary's cached state for the account is dropped after the commit, so
	// the very next request sees the new flag (the TTL is only the
	// CROSS-instance bound — account_cache.go).
	s.accounts.invalidate(username)
	if changed {
		action, detail := AuditUserReactivate, ""
		if disabled {
			action, detail = AuditUserDeactivate, reason
		}
		s.auditAdmin(ctx, action, "user:"+username, detail)
	}
	return result, nil
}

// errStopScan ends a store listing once the page is full (the callback error
// aborts the stream; the implementations propagate it).
var errStopScan = errors.New("directory: scan bound reached")

// directoryUsernames streams account directory names under users/ in name
// order, starting strictly after `after`, calling fn for each. It skips the
// reserved by-email alias family (users/by-email/ is an index, not an
// account) and any name that is not a legal username.
func (s *Service) directoryUsernames(ctx context.Context, after string, fn func(string) error) error {
	after = normPrincipal(after)
	return s.Store.ListPrefixes(ctx, "users/", func(p string) error {
		name := strings.TrimSuffix(strings.TrimPrefix(p, "users/"), "/")
		if name == "by-email" || !auth.ValidUsername(name) {
			return nil
		}
		if after != "" && name <= after {
			return nil
		}
		return fn(name)
	})
}

// ListDirectoryUsers answers one page of the users directory. Without a query
// it walks names only (a delimiter LIST) until the page is full. With a query
// it examines at most DirectoryScanBound accounts — username prefix/substring
// and display-name substring, case-insensitive — and reports whether it
// stopped early. Each examined account costs its user.json and profile.json
// read; a returned row reuses the reads that matched it.
func (s *Service) ListDirectoryUsers(ctx context.Context, q DirectoryQuery) (*DirectoryUserPage, error) {
	limit := q.limit()
	query := strings.ToLower(strings.TrimSpace(q.Query))
	page := &DirectoryUserPage{Rows: []DirectoryUser{}}
	if query == "" {
		var matches []string
		scanErr := s.directoryUsernames(ctx, q.After, func(name string) error {
			page.Examined++
			matches = append(matches, name)
			if len(matches) > limit {
				return errStopScan
			}
			return nil
		})
		if scanErr != nil && !errors.Is(scanErr, errStopScan) {
			return nil, scanErr
		}
		if len(matches) > limit {
			page.Next = matches[limit-1]
			matches = matches[:limit]
		}
		for _, name := range matches {
			row, err := s.directoryUser(ctx, name)
			if err != nil {
				return nil, err
			}
			page.Rows = append(page.Rows, row)
		}
		return page, nil
	}

	page.Bound = DirectoryScanBound
	seen := map[string]DirectoryUser{}
	var matches []string
	last := ""
	stop := "" // "page" | "scan"
	scanErr := s.directoryUsernames(ctx, q.After, func(name string) error {
		if page.Examined >= DirectoryScanBound {
			stop = "scan"
			return errStopScan
		}
		page.Examined++
		last = name
		row, err := s.directoryUser(ctx, name)
		if err != nil {
			return err
		}
		if strings.Contains(name, query) || strings.Contains(strings.ToLower(row.DisplayName), query) {
			seen[name] = row
			matches = append(matches, name)
			if len(matches) > limit {
				stop = "page"
				return errStopScan
			}
		}
		return nil
	})
	if scanErr != nil && !errors.Is(scanErr, errStopScan) {
		return nil, scanErr
	}
	if len(matches) > limit {
		page.Next = matches[limit-1]
		matches = matches[:limit]
	} else if stop == "scan" && last != "" {
		// The bound ended the walk with room to spare: the next request
		// continues after the last name this one examined.
		page.Next = last
		page.Truncated = true
	}
	for _, name := range matches {
		row, ok := seen[name]
		if !ok {
			var err error
			if row, err = s.directoryUser(ctx, name); err != nil {
				return nil, err
			}
		}
		page.Rows = append(page.Rows, row)
	}
	return page, nil
}

// directoryUser reads one directory row: the account binding (state) and the
// profile (display name). A missing profile leaves DisplayName empty; a missing
// user.json leaves Bound=false.
func (s *Service) directoryUser(ctx context.Context, name string) (DirectoryUser, error) {
	row := DirectoryUser{Username: name}
	raw, _, err := store.GetBytes(ctx, s.Store, UserKey(name), store.GetOptions{})
	if err != nil {
		if !store.IsNotFound(err) {
			return row, err
		}
	} else {
		u, perr := parseUserDoc(raw)
		if perr != nil {
			return row, perr
		}
		row.Bound = true
		row.Disabled = u.Disabled
		row.DisabledBy = u.DisabledBy
		row.DisabledAt = u.DisabledAt
		row.Reason = u.Reason
		row.TokensRevokedAt = u.TokensRevokedAt
	}
	p, perr := s.GetProfile(ctx, name)
	if perr != nil {
		return row, perr
	}
	if p != nil {
		row.DisplayName = p.DisplayName
	}
	return row, nil
}

// ListDirectoryOrgs answers one page of the organizations directory: one
// delimiter LIST over orgs/ plus one org.json and one members.json read per
// row (the roster is what makes the member count cheap).
func (s *Service) ListDirectoryOrgs(ctx context.Context, q DirectoryQuery) (*DirectoryOrgPage, error) {
	limit := q.limit()
	after := normPrincipal(q.After)
	page := &DirectoryOrgPage{Rows: []DirectoryOrg{}}
	var names []string
	scanErr := s.Store.ListPrefixes(ctx, "orgs/", func(p string) error {
		name := strings.TrimSuffix(strings.TrimPrefix(p, "orgs/"), "/")
		if !ValidOrg(name) {
			return nil
		}
		if after != "" && name <= after {
			return nil
		}
		page.Scanned++
		names = append(names, name)
		if len(names) > limit {
			return errStopScan
		}
		return nil
	})
	if scanErr != nil && !errors.Is(scanErr, errStopScan) {
		return nil, scanErr
	}
	if len(names) > limit {
		page.Next = names[limit-1]
		names = names[:limit]
	}
	for _, name := range names {
		row := DirectoryOrg{Name: name}
		raw, _, err := store.GetBytes(ctx, s.Store, OrgKey(name), store.GetOptions{})
		if err != nil {
			if !store.IsNotFound(err) {
				return nil, err
			}
		} else if o, perr := parseOrg(raw); perr != nil {
			return nil, perr
		} else {
			row.DisplayName = o.DisplayName
		}
		m, _, err := s.getMembers(ctx, name)
		if err != nil {
			return nil, err
		}
		if m != nil {
			row.Members = len(m.Members)
		}
		page.Rows = append(page.Rows, row)
	}
	return page, nil
}

// OrgRolesFor reports, for the named principals, the organizations they belong
// to and whether the roster role is owner. It is one bounded walk over orgs/
// (DirectoryOrgScanBound organizations, each one members.json read) shared by
// every row of a directory page — never a walk per row — and the same call
// answers "which orgs does this account own" before/after deactivation.
// truncated reports that the walk stopped at the bound (the caller says so).
func (s *Service) OrgRolesFor(ctx context.Context, principals []string) (map[string][]OrgMembership, bool, error) {
	out := map[string][]OrgMembership{}
	want := make([]auth.Principal, 0, len(principals))
	for _, p := range principals {
		p = normPrincipal(p)
		if p == "" {
			continue
		}
		want = append(want, auth.Principal{Name: p})
	}
	if len(want) == 0 {
		return out, false, nil
	}
	// A username spelling also matches roster rows held under its verified
	// email (the #370 alias rule the user-orgs rail applies).
	for i := range want {
		if em, err := s.EmailForUsername(ctx, want[i].Name); err == nil && em != "" {
			want[i].Email = em
		}
	}
	var orgs []string
	scanErr := s.Store.ListPrefixes(ctx, "orgs/", func(p string) error {
		name := strings.TrimSuffix(strings.TrimPrefix(p, "orgs/"), "/")
		if !ValidOrg(name) {
			return nil
		}
		orgs = append(orgs, name)
		if len(orgs) >= DirectoryOrgScanBound {
			return errStopScan
		}
		return nil
	})
	if scanErr != nil && !errors.Is(scanErr, errStopScan) {
		return nil, false, scanErr
	}
	truncated := errors.Is(scanErr, errStopScan)
	sort.Strings(orgs)
	for _, org := range orgs {
		m, _, err := s.getMembers(ctx, org)
		if err != nil {
			return nil, false, err
		}
		if m == nil {
			continue
		}
		for _, e := range m.Members {
			for _, p := range want {
				if matchPrincipal(e.Principal, p) {
					out[p.Name] = append(out[p.Name], OrgMembership{Org: org, Owner: e.Role == OrgOwner})
					break
				}
			}
		}
	}
	return out, truncated, nil
}
