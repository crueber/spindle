package moderation

// auditcanary_test.go — the audit-completeness sweep (PLAN-V2 §M.4/M.6.5).
//
// The sibling of gateverbs_test.go: where that canary proves every
// collaboration mutation verb consults a block gate, this one proves every
// moderation/administrative mutation writes an audit entry. It walks the
// moderation package's *Service AST and requires each mutating verb to call the
// audit writer (directly or through a verb it delegates to); anything that
// legitimately does not audit carries an explicit allow-list row with a reason.
//
// A second, fixed table names the administrative mutations that live OUTSIDE
// this package (the config save, token mint, the identity role grants and team
// memberships, the repo transfer, the packages-GC trigger). Each row is either
// verified by finding the audit call in the named function, or allow-listed
// with the reason (only the self-service token mint remains: its revocation is
// a config save, which audits). A row whose function disappeared fails loudly
// instead of ageing into a phantom claim. A third table pins the card's five
// required audit families (config save, token mint/revoke, admin grant, repo
// transfer, moderation) to the function that must carry the call, and its
// negative test drops each family's call in turn.
//
// The failure text names the verb, so the fix is the verb and not a hunt.

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// auditWriters are the calls that constitute "this verb audits".
var auditWriters = []string{"s.appendAudit(", "s.audit("}

// mutationCalls are the calls that make a verb a MUTATION (it writes state).
// A verb that calls one of these and no audit writer must be allow-listed.
var mutationCalls = []string{
	"s.mutate(",
	"s.mutateDoc(",
	"s.mutateReportsIndexDoc(",
	"s.mutateGlobalIndex(",
	"s.appendReport(",
	"store.PutBytes(",
	"s.appendAudit(",
	"s.reserveAuditSeq(",
}

// auditExempt lists moderation mutation verbs that legitimately do not write an
// audit record, with the plan-derived reason. No phantom rows: the test
// requires every entry to name a collected verb.
var auditExempt = map[string]string{
	"FileReport": "the filing IS the evidence (a Create-only report record) plus the report_filed " +
		"notification and SSE frame; PLAN-V2 §M.2's audit list names terminal report actions only",
	"NoteBody": "advisory spam observation: its only effect is a FileReport through the same M3 tail, " +
		"which is already the evidence record (a machine signal is not an administrative act)",
	"CompactReports": "the maintainer-driven hot-window rebuild: it evicts resolved cards and rebuilds " +
		"the global projection, changes no moderation decision and has no actor to name",
}

func TestEveryModerationMutationVerbAudits(t *testing.T) {
	bodies := moderationVerbBodies(t)
	collected := make([]string, 0, len(bodies))
	for name := range bodies {
		collected = append(collected, name)
	}
	sort.Strings(collected)

	// Every exempt row names a real verb.
	for name, reason := range auditExempt {
		if _, ok := bodies[name]; !ok {
			t.Fatalf("audit allow-list names %q, which is not a *Service verb (stale row)", name)
		}
		if strings.TrimSpace(reason) == "" {
			t.Fatalf("audit allow-list entry %q has no reason", name)
		}
	}

	if gaps := auditCoverageGaps(bodies, auditExempt); len(gaps) > 0 {
		t.Fatalf("%v mutate moderation state but write no audit entry: call s.audit/s.appendAudit, "+
			"delegate to an audited verb, or add an auditExempt row with the reason", gaps)
	}
}

// auditCoverageGaps returns the mutation verbs that neither audit nor delegate
// to an audited mutation nor carry an allow-list reason. Kept as a function so
// TestAuditCanaryHasTeeth can prove the sweep rejects a synthetic gap instead of
// trusting a green run over today's (compliant) tree.
func auditCoverageGaps(bodies map[string]string, exempt map[string]string) []string {
	var gaps []string
	for name, body := range bodies {
		if !isMutation(body) || audits(body) {
			continue
		}
		if delegatesToAudited(body, bodies, map[string]bool{}) {
			continue
		}
		if _, ok := exempt[name]; ok {
			continue
		}
		gaps = append(gaps, name)
	}
	sort.Strings(gaps)
	return gaps
}

// TestAuditCanaryHasTeeth is the negative control: a mutation verb with no
// audit call (and no allow-list row) must be named, and the delegation rule
// must accept a verb that forwards to an audited one.
func TestAuditCanaryHasTeeth(t *testing.T) {
	gaps := auditCoverageGaps(map[string]string{
		"FakeMutate":  "func (s *Service) FakeMutate() { s.mutateDoc(context.Background(), \"o\", \"r\", nil) }",
		"FakeAudits":  "func (s *Service) FakeAudits() { s.audit(context.Background(), \"a\", \"hide\", \"o\", \"r\", \"\", \"\") }",
		"FakePure":    "func (s *Service) FakePure() { return }",
		"FakeDelegat": "func (s *Service) FakeDelegat() { s.FakeAudits() }",
		"FakeExempt":  "func (s *Service) FakeExempt() { s.appendReport(context.Background(), \"o\", \"r\", \"x\", \"user\", \"user:x\", \"spam\", \"\") }",
	}, map[string]string{"FakeExempt": "the filing is the evidence"})
	if len(gaps) != 1 || gaps[0] != "FakeMutate" {
		t.Fatalf("gaps = %v, want [FakeMutate]", gaps)
	}
}

// isMutation reports whether a verb body writes state.
func isMutation(body string) bool {
	for _, c := range mutationCalls {
		if strings.Contains(body, c) {
			return true
		}
	}
	return false
}

func audits(body string) bool {
	for _, c := range auditWriters {
		if strings.Contains(body, c) {
			return true
		}
	}
	return false
}

// delegatesToAudited follows s.<Verb>( calls (one hop per verb, cycle-safe) to
// a verb that audits — Apply → audit, PatchIssue → the writer, etc.
func delegatesToAudited(body string, bodies map[string]string, seen map[string]bool) bool {
	for name, impl := range bodies {
		if seen[name] || !isMutation(impl) {
			continue // only another mutation verb can carry the audit obligation
		}
		if !strings.Contains(body, "s."+name+"(") {
			continue
		}
		seen[name] = true
		if audits(impl) || delegatesToAudited(impl, bodies, seen) {
			return true
		}
	}
	return false
}

// moderationVerbBodies returns every exported *Service method's printed body.
func moderationVerbBodies(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", path, perr)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Name == nil || !fn.Name.IsExported() {
				continue
			}
			if !receiverIsService(fn) { // shared with gateverbs_test.go
				continue
			}
			var b strings.Builder
			if err := printer.Fprint(&b, fset, fn); err != nil {
				t.Fatal(err)
			}
			out[fn.Name.Name] = b.String()
		}
	}
	if len(out) == 0 {
		t.Fatal("no *Service verbs collected — the canary scanned nothing")
	}
	return out
}

// --- administrative mutations outside this package -----------------------------

// adminAuditSite is one administrative mutation outside internal/moderation.
type adminAuditSite struct {
	path   string // relative to this package's directory
	fn     string // function name
	marker string // required audit call text in the body; "" means allow-listed
	reason string // non-empty ⇒ allow-listed, with the reason and its owner
}

// adminAuditSites is the closed table. Every row must be FOUND (the function
// exists) and either carry the audit call or an explicit reason.
var adminAuditSites = []adminAuditSite{
	{
		// The Administration area's one mutation: saving spindle.toml.
		path: "../webui/admin.go", fn: "adminConfigSubmit", marker: "d.AuditWrite(",
	},
	{
		// The exported entry to the audit family itself.
		path: "audit.go", fn: "RecordAdmin", marker: "s.appendAudit(",
	},
	{
		// Minting one's own API token: a credential for oneself, with no
		// effect on anyone else's content or access. Revocation is
		// session_secret rotation — a config save, which audits. Recorded in
		// docs/features/16_moderation.md §14.
		path: "../server/auth_oidc.go", fn: "authTokensMint",
		reason: "self-service credential mint (no state about others); revocation = session_secret rotation, " +
			"which is a config_save and audits",
	},
	{
		// Repo role/binding grants (who may write what) — the "admin grant"
		// family. Writes access_grant; the actor rides the request context
		// (identity.WithActor).
		path: "../identity/access.go", fn: "PutAccess", marker: "s.auditAdmin(",
	},
	{
		// Org membership/role grants (org admins) — same family.
		path: "../identity/orgs.go", fn: "SetMember", marker: "s.auditAdmin(",
	},
	{
		path: "../identity/orgs.go", fn: "RemoveMember", marker: "s.auditAdmin(",
	},
	{
		// Team membership: a team binding is how a repo role reaches people,
		// so the grant belongs beside access.json's.
		path: "../identity/orgs.go", fn: "SetTeamMember", marker: "s.auditAdmin(",
	},
	{
		path: "../identity/orgs.go", fn: "RemoveTeamMember", marker: "s.auditAdmin(",
	},
	{
		// The v1 direct repo transfer (the CLI/test shape) and the task body
		// both end in transferRun, which writes the `transfer` record after
		// verification (PLAN-V2 §T.4).
		path: "../identity/transfer.go", fn: "TransferRepo", marker: "s.transferRun(",
	},
	{
		path: "../identity/transfer.go", fn: "transferRun", marker: "s.auditAdmin(",
	},
	{
		// The instance directory's deactivate/reactivate transition
		// (PLAN-ADMIN §3.7): one record per actual state change, actor from
		// identity.WithActor. A re-submit of the same state writes nothing.
		path: "../identity/directory.go", fn: "SetUserDisabled", marker: "s.auditAdmin(",
	},
	{
		// The instance packages-GC trigger (PLAN-ADMIN §8.6): every trigger —
		// dry run, apply, join — writes `packages_gc` at the host target.
		path: "../packages/gcadmin.go", fn: "StartGC", marker: "s.auditGC(",
	},
	{
		path: "../packages/gcadmin.go", fn: "auditGC", marker: "s.Audit(",
	},
}

// auditFamily pins the card's five required audit families (PLAN-ADMIN §8.6)
// to the function that must carry the audit call. The family sweep is the
// negative contract: if any family's anchor stops auditing, this canary fails
// even when the rest of the table is green.
type auditFamily struct {
	name   string
	path   string
	fn     string
	marker string
	note   string
}

var auditFamilies = []auditFamily{
	{name: "config save", path: "../webui/admin.go", fn: "adminConfigSubmit", marker: "d.AuditWrite("},
	{name: "token mint/revoke", path: "../webui/admin.go", fn: "adminConfigSubmit", marker: "d.AuditWrite(",
		note: "mint is self-service (authTokensMint's allow-list row); revocation is session_secret rotation, a config_save"},
	{name: "admin grant", path: "../identity/access.go", fn: "PutAccess", marker: "s.auditAdmin("},
	{name: "repo transfer", path: "../identity/transfer.go", fn: "transferRun", marker: "s.auditAdmin("},
	{name: "moderation", path: "hide.go", fn: "Hide", marker: "s.audit("},
	{name: "packages GC", path: "../packages/gcadmin.go", fn: "StartGC", marker: "s.auditGC("},
}

// auditFamilyGaps returns the families whose anchor function does not carry its
// audit call. Kept pure so TestAuditFamiliesHaveTeeth can prove the check
// rejects a synthetic gap instead of trusting today's tree.
func auditFamilyGaps(families []auditFamily, body func(path, fn string) (string, bool)) []string {
	var gaps []string
	for _, f := range families {
		b, ok := body(f.path, f.fn)
		if !ok || !strings.Contains(b, f.marker) {
			gaps = append(gaps, f.name)
		}
	}
	return gaps
}

// TestAuditFamiliesAudit proves each required family has a live auditing anchor.
func TestAuditFamiliesAudit(t *testing.T) {
	body := func(path, fn string) (string, bool) { return functionBody(t, path, fn) }
	for _, f := range auditFamilies {
		if _, ok := body(f.path, f.fn); !ok {
			t.Fatalf("audit family %q names %s in %s, which does not exist", f.name, f.fn, f.path)
		}
	}
	if gaps := auditFamilyGaps(auditFamilies, body); len(gaps) > 0 {
		t.Fatalf("audit families without an auditing anchor: %v", gaps)
	}
}

// TestAuditFamiliesHaveTeeth is the card's negative test: dropping the audit
// call from any one anchor names every family that rides it (families may share
// an anchor — token revocation IS the config save) and nothing else.
func TestAuditFamiliesHaveTeeth(t *testing.T) {
	for _, drop := range auditFamilies {
		want := map[string]bool{}
		for _, f := range auditFamilies {
			if f.path == drop.path && f.fn == drop.fn {
				want[f.name] = true
			}
		}
		gaps := auditFamilyGaps(auditFamilies, func(path, fn string) (string, bool) {
			body, ok := functionBody(t, path, fn)
			if !ok {
				return "", false
			}
			if path == drop.path && fn == drop.fn {
				return strings.ReplaceAll(body, drop.marker, ""), true
			}
			return body, true
		})
		if len(gaps) != len(want) {
			t.Fatalf("dropping the audit call from family %q gave gaps %v, want %d", drop.name, gaps, len(want))
		}
		for _, g := range gaps {
			if !want[g] {
				t.Fatalf("dropping the audit call from family %q named unrelated family %q", drop.name, g)
			}
		}
	}
	// A family whose function vanished is a gap, never a silent pass.
	if gaps := auditFamilyGaps(auditFamilies, func(string, string) (string, bool) { return "", false }); len(gaps) != len(auditFamilies) {
		t.Fatalf("missing functions gave %d gaps, want %d", len(gaps), len(auditFamilies))
	}
}

func TestEveryAdministrativeMutationAudits(t *testing.T) {
	for _, site := range adminAuditSites {
		body, ok := functionBody(t, site.path, site.fn)
		if !ok {
			t.Fatalf("audit sweep names %s in %s, which does not exist (stale row)", site.fn, site.path)
		}
		if site.marker != "" {
			if !strings.Contains(body, site.marker) {
				t.Fatalf("%s mutates administrative state but never calls %s", site.fn, site.marker)
			}
			if site.reason != "" {
				t.Fatalf("%s has both an audit call and an exempt reason; keep one", site.fn)
			}
			continue
		}
		if strings.TrimSpace(site.reason) == "" {
			t.Fatalf("%s neither audits nor carries a reason", site.fn)
		}
	}
}

// functionBody parses path and returns the printed body of funcName.
func functionBody(t *testing.T, path, fnName string) (string, bool) {
	t.Helper()
	src, err := os.ReadFile(filepath.FromSlash(path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	fset := token.NewFileSet()
	f, perr := parser.ParseFile(fset, path, src, 0)
	if perr != nil {
		t.Fatalf("parse %s: %v", path, perr)
	}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != fnName {
			continue
		}
		var b strings.Builder
		if err := printer.Fprint(&b, fset, fn); err != nil {
			t.Fatal(err)
		}
		return b.String(), true
	}
	return "", false
}
