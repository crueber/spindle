package moderation

// gateverbs_test.go — the moderation gate drift canary (PLAN-V2 §M.6.1,
// §M.12: "a missed collab verb is a block bypass"). It greps the five
// collaboration packages for exported Service methods that take a principal
// and asserts every one is either:
//
//   - gated: its body calls the package's requireNotBlocked helper,
//   - delegated: it forwards to another verb (which must itself be gated),
//   - exempt: listed with a written reason the plan itself states.
//
// A new mutation verb, or a gated verb that loses its call, fails this test.
// It is deliberately source-shaped: a behavioural test can only prove the
// verbs we remembered to call, which is the blind spot this canary closes.

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

type gateVerbKind int

const (
	verbGated gateVerbKind = iota
	verbDelegates
	verbExempt
)

type gateVerb struct {
	pkg    string // issues | pulls | review | social | notify
	name   string
	kind   gateVerbKind
	target string // for verbDelegates: the verb it forwards to
	reason string // for verbExempt: why the plan does not gate it
}

// gatedVerbs is the closed table. Every collected method must appear; every
// entry must be collected (no phantoms).
var gatedVerbs = []gateVerb{
	// issues — interaction with a thread/conversation in the blocker's repo.
	{pkg: "issues", name: "CreateIssue", kind: verbGated},
	{pkg: "issues", name: "AddComment", kind: verbGated},
	{pkg: "issues", name: "AddReaction", kind: verbGated},
	{pkg: "issues", name: "UploadAttachment", kind: verbGated},
	{pkg: "issues", name: "PatchIssue", kind: verbDelegates, target: "patchThread"},
	{pkg: "issues", name: "PatchThreadMetadata", kind: verbDelegates, target: "patchThread"},
	{pkg: "issues", name: "RemoveReaction", kind: verbExempt,
		reason: "removing one's OWN reaction is a personal undo, not interaction (#594 precedent: it is deliberately not even lock-gated)"},
	{pkg: "issues", name: "CreateLabel", kind: verbExempt, reason: "repo configuration, governed by the triage role floor; not content interaction"},
	{pkg: "issues", name: "UpdateLabel", kind: verbExempt, reason: "repo configuration, governed by the triage role floor"},
	{pkg: "issues", name: "DeleteLabel", kind: verbExempt, reason: "repo configuration, governed by the triage role floor"},
	{pkg: "issues", name: "CreateMilestone", kind: verbExempt, reason: "repo configuration, governed by the triage/write role floor"},
	{pkg: "issues", name: "UpdateMilestone", kind: verbExempt, reason: "repo configuration, governed by the triage/write role floor"},
	{pkg: "issues", name: "DeleteMilestone", kind: verbExempt, reason: "repo configuration, governed by the triage/write role floor"},
	// §13/§14 curation (pins, dependency links): repo configuration, like
	// labels/milestones — exempt by the 16_moderation §3.3 rule.
	{pkg: "issues", name: "PinIssue", kind: verbExempt, reason: "repo configuration (pin set), triage floor"},
	{pkg: "issues", name: "AddDependency", kind: verbExempt, reason: "repo configuration (dependency links), triage floor"},
	{pkg: "issues", name: "RemoveDependency", kind: verbExempt, reason: "repo configuration (dependency links), triage floor"},
	// reads that happen to take a principal
	{pkg: "issues", name: "ListIssues", kind: verbExempt, reason: "read"},
	{pkg: "issues", name: "GetThread", kind: verbExempt, reason: "read"},
	{pkg: "issues", name: "DependenciesFor", kind: verbExempt, reason: "read"},

	// pulls — opening/updating/merging is interaction; forks are not (§M.5).
	{pkg: "pulls", name: "OpenPR", kind: verbGated},
	{pkg: "pulls", name: "UpdatePR", kind: verbGated},
	{pkg: "pulls", name: "AddComment", kind: verbGated},
	{pkg: "pulls", name: "StartMerge", kind: verbGated},
	{pkg: "pulls", name: "UpdateBranch", kind: verbGated},
	{pkg: "pulls", name: "DeleteHead", kind: verbGated},
	{pkg: "pulls", name: "StartFork", kind: verbExempt, reason: "PLAN-V2 §M.5: forks are explicitly unaffected by a block"},
	{pkg: "pulls", name: "ListForks", kind: verbExempt, reason: "read"},
	{pkg: "pulls", name: "ListPRs", kind: verbExempt, reason: "read"},
	{pkg: "pulls", name: "GetPR", kind: verbExempt, reason: "read"},
	{pkg: "pulls", name: "Diff", kind: verbExempt, reason: "read"},
	{pkg: "pulls", name: "Commits", kind: verbExempt, reason: "read"},

	// review — every write posts conversation activity.
	{pkg: "review", name: "SubmitReview", kind: verbGated},
	{pkg: "review", name: "DismissReview", kind: verbGated},
	{pkg: "review", name: "OpenThread", kind: verbGated},
	{pkg: "review", name: "AddThreadComment", kind: verbGated},
	{pkg: "review", name: "ResolveThread", kind: verbGated},
	{pkg: "review", name: "UnresolveThread", kind: verbGated},
	{pkg: "review", name: "AddRequests", kind: verbGated},
	{pkg: "review", name: "RemoveRequests", kind: verbGated},
	{pkg: "review", name: "ListReviews", kind: verbExempt, reason: "read"},
	{pkg: "review", name: "GetReview", kind: verbExempt, reason: "read"},
	{pkg: "review", name: "ListThreads", kind: verbExempt, reason: "read"},
	{pkg: "review", name: "GetThread", kind: verbExempt, reason: "read"},
	{pkg: "review", name: "GetRequests", kind: verbExempt, reason: "read"},
	{pkg: "review", name: "Suggest", kind: verbExempt, reason: "read"},

	// social — stars/watches are explicitly unaffected (§M.5).
	{pkg: "social", name: "Star", kind: verbExempt, reason: "PLAN-V2 §M.5: stars are unaffected by a block"},
	{pkg: "social", name: "Unstar", kind: verbExempt, reason: "PLAN-V2 §M.5: stars are unaffected"},
	{pkg: "social", name: "Counts", kind: verbExempt, reason: "read"},
	{pkg: "social", name: "ViewerState", kind: verbExempt, reason: "read"},
	{pkg: "social", name: "Starred", kind: verbExempt, reason: "read"},

	// notify — the mention filter is the gate here, not the watch verb.
	{pkg: "notify", name: "SetWatch", kind: verbExempt, reason: "PLAN-V2 §M.5: watches are unaffected; the mention filter is the notify gate"},
	{pkg: "notify", name: "GetWatch", kind: verbExempt, reason: "read"},
	{pkg: "notify", name: "Tray", kind: verbExempt, reason: "read"},
	{pkg: "notify", name: "UnreadCount", kind: verbExempt, reason: "read"},
	{pkg: "notify", name: "ReadAll", kind: verbExempt, reason: "personal notification state, not interaction with content"},
	{pkg: "notify", name: "SetState", kind: verbExempt, reason: "personal notification state"},
}

func TestEveryMutationVerbIsGated(t *testing.T) {
	collected := scanMutationVerbs(t)

	table := map[string]gateVerb{}
	for _, v := range gatedVerbs {
		key := v.pkg + "." + v.name
		if _, dup := table[key]; dup {
			t.Fatalf("duplicate table entry %s", key)
		}
		table[key] = v
	}
	// Every collected verb is in the table (a new verb fails here).
	var missing []string
	for _, key := range collected {
		if _, ok := table[key]; !ok {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("unguarded mutation verbs (add a gate or an exempt reason): %v", missing)
	}
	// Every table entry is collected (no phantom claims of coverage).
	for key := range table {
		if !contains(collected, key) {
			t.Fatalf("table entry %s matches no Service method (stale canary row)", key)
		}
	}

	// Gated verbs must actually call the helper; delegated ones must forward
	// to a gated verb.
	bodies := scanBodies(t)
	for _, v := range gatedVerbs {
		key := v.pkg + "." + v.name
		body := bodies[key]
		switch v.kind {
		case verbGated:
			if !strings.Contains(body, "requireNotBlocked(") {
				t.Fatalf("%s is marked gated but never calls requireNotBlocked", key)
			}
		case verbDelegates:
			if !strings.Contains(body, "s."+v.target+"(") {
				t.Fatalf("%s is marked delegating to %s but does not call it", key, v.target)
			}
			if !strings.Contains(bodies[v.pkg+"."+v.target], "requireNotBlocked(") {
				t.Fatalf("%s delegates to %s, which is not itself gated", key, v.target)
			}
		case verbExempt:
			if v.reason == "" {
				t.Fatalf("%s is exempt without a reason", key)
			}
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// scanMutationVerbs parses the five packages and returns
// "pkg.Name" for every exported *Service method that takes a principal.
func scanMutationVerbs(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, pkg := range []string{"issues", "pulls", "review", "social", "notify"} {
		dir := filepath.Join("..", pkg)
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
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
				if !receiverIsService(fn) || !takesPrincipal(fset, fn) {
					continue
				}
				out = append(out, pkg+"."+fn.Name.Name)
			}
		}
	}
	return out
}

// scanBodies returns "pkg.Name" → the declaration source, so the assertions
// can look for the helper call.
func scanBodies(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, pkg := range []string{"issues", "pulls", "review", "social", "notify"} {
		files, _ := filepath.Glob(filepath.Join("..", pkg, "*.go"))
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
				if !ok || fn.Recv == nil || fn.Name == nil || !receiverIsService(fn) {
					continue
				}
				var b strings.Builder
				if err := printer.Fprint(&b, fset, fn); err != nil {
					t.Fatal(err)
				}
				out[pkg+"."+fn.Name.Name] = b.String()
			}
		}
	}
	return out
}

func receiverIsService(fn *ast.FuncDecl) bool {
	if len(fn.Recv.List) != 1 {
		return false
	}
	star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := star.X.(*ast.Ident)
	return ok && id.Name == "Service"
}

// takesPrincipal matches methods taking auth.Principal, or the notify shape
// where the principal is a string parameter literally named principal/who
// (SetWatch, Tray, ReadAll, SetState).
func takesPrincipal(fset *token.FileSet, fn *ast.FuncDecl) bool {
	for _, field := range fn.Type.Params.List {
		var b strings.Builder
		if err := printer.Fprint(&b, fset, field.Type); err != nil {
			continue
		}
		typ := b.String()
		if strings.Contains(typ, "auth.Principal") {
			return true
		}
		if typ == "string" {
			for _, name := range field.Names {
				if name.Name == "principal" || name.Name == "who" {
					return true
				}
			}
		}
	}
	return false
}

func TestPackageDirsExist(t *testing.T) {
	for _, pkg := range []string{"issues", "pulls", "review", "social", "notify"} {
		if _, err := os.Stat(filepath.Join("..", pkg)); err != nil {
			t.Fatalf("canary cannot see ../%s: %v", pkg, err)
		}
	}
}
