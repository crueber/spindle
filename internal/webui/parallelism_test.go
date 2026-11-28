package webui

// parallelism_test.go — the executable form of the webui parallelisation rule
// (docs/go/15_testing.md §6.7).
//
// The page seams are process globals by design (server.go: one provider per
// process), so a test that installs one — or that rewrites a product knob the
// handlers read — is NOT independent: a peer calling t.Parallel() would read
// the value mid-test. Go's testing package runs every parallel top-level test
// only after the last sequential test returns, so the rule is exactly "a
// parallel test (transitively) binds nothing"; that keeps the parallel batch
// and the binding tests out of each other's window without a mutex.
//
// Reading the rule out of the source is the only way to keep it true as the
// suite grows: a harness that starts binding later would otherwise turn a
// t.Parallel() into a race that only shows up in an unlucky schedule.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// packageGlobals are the process-wide bindings and knobs a test may install.
// Binding any of them makes a test sequential.
var packageGlobals = []string{
	"adminBound", "collabDeps", "webeditDeps", "formsDeps", "packagesDeps",
	"publicDeps", "queueDeps", "repoDeps", "settingsBound", "threadsDeps",
	"adminRestartDelay", "assetUploadBodyLimit", "runnerTenancy",
}

// binderFuncs are the setters that write those globals.
var binderFuncs = []string{
	"BindAdmin", "BindSettings", "BindCollab", "BindForms", "BindPackages",
	"BindPublic", "BindQueue", "BindRepo", "BindThreads", "BindWebedit", "BindDirectory",
	"bindInsights", "BindAdminRunnerTenancy",
}

// testFunc is one function/method declared in the package's test files.
type testFunc struct {
	name     string
	file     string
	parallel bool
	binds    string // non-empty: the call/assignment that binds
	calls    []string
}

func TestParallelTestsBindNoGlobals(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*_test.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob test files: %v (%d)", err, len(files))
	}
	if len(files) < 2 {
		t.Fatalf("only %d test file(s) — run this from the package dir", len(files))
	}
	fset := token.NewFileSet()
	decls := map[string]*testFunc{}
	for _, path := range files {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			entry := &testFunc{name: fd.Name.Name, file: path}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CallExpr:
					name := calleeName(n.Fun)
					if name == "" {
						return true
					}
					entry.calls = append(entry.calls, name)
					if name == "Parallel" && isSelector(n.Fun) {
						entry.parallel = true
					}
					if entry.binds == "" {
						for _, b := range binderFuncs {
							if name == b {
								entry.binds = b + "()"
							}
						}
					}
				case *ast.AssignStmt:
					if entry.binds != "" {
						return true
					}
					for _, lhs := range n.Lhs {
						if g := globalRoot(lhs); g != "" {
							entry.binds = g + "… = …"
						}
					}
				}
				return true
			})
			decls[entry.name] = entry
		}
	}
	// Transitive closure: a parallel test reaching a binder through a helper
	// (a fixture, a shared server constructor, a fixture method) is still not
	// independent.
	var pathToBind func(name string, seen map[string]bool) string
	pathToBind = func(name string, seen map[string]bool) string {
		entry, ok := decls[name]
		if !ok || seen[name] {
			return ""
		}
		seen[name] = true
		if entry.binds != "" {
			return name + ": " + entry.binds
		}
		calls := append([]string(nil), entry.calls...)
		sort.Strings(calls)
		for _, c := range calls {
			if c == name {
				continue
			}
			if via := pathToBind(c, seen); via != "" {
				return c + " → " + via
			}
		}
		return ""
	}
	parallel := 0
	for name, entry := range decls {
		if !entry.parallel || !strings.HasPrefix(name, "Test") {
			continue
		}
		parallel++
		if name == "TestParallelTestsBindNoGlobals" {
			continue
		}
		if via := pathToBind(name, map[string]bool{}); via != "" {
			t.Errorf("%s (%s) calls t.Parallel() but reaches a package-global binding: %s\n"+
				"Bindings are process-wide (one provider per process), so this test is not "+
				"independent: drop t.Parallel() or move the binding into a sequential test "+
				"(docs/go/15_testing.md §6.7).", name, entry.file, via)
		}
	}
	if parallel == 0 {
		t.Fatal("no parallel tests found — the guard is not looking at the suite")
	}
	// The lists above are the contract, so keep them in step with the package:
	// a new package-level Bind* or *Deps global that nobody classified would
	// let a parallel test start binding silently.
	checkBindingInventory(t)
	t.Logf("%d parallel top-level tests; none binds a package global", parallel)
}

// checkBindingInventory fails when the product grows a package-level binder or
// a *Deps/Bound global the guard does not know about.
func checkBindingInventory(t *testing.T) {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob sources: %v", err)
	}
	fset := token.NewFileSet()
	known := func(list []string, name string) bool {
		for _, n := range list {
			if n == name {
				return true
			}
		}
		return false
	}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				if n.Recv != nil || !strings.HasPrefix(n.Name.Name, "Bind") {
					return true
				}
				if !known(binderFuncs, n.Name.Name) {
					t.Errorf("%s: %s is a package-level binder missing from binderFuncs in "+
						"parallelism_test.go — add it so a t.Parallel() test cannot bind it",
						path, n.Name.Name)
				}
			case *ast.ValueSpec:
				for _, id := range n.Names {
					if !strings.HasSuffix(id.Name, "Deps") && !strings.HasSuffix(id.Name, "Bound") {
						continue
					}
					if !known(packageGlobals, id.Name) {
						t.Errorf("%s: %s is a package-level binding missing from packageGlobals in "+
							"parallelism_test.go — add it so a t.Parallel() test cannot write it",
							path, id.Name)
					}
				}
			}
			return true
		})
	}
}

// calleeName is the called function's name for an identifier or a selector
// (`BindRepo(...)` and `f.deps(...)` both resolve by their last segment).
func calleeName(fun ast.Expr) string {
	switch e := fun.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	}
	return ""
}

func isSelector(e ast.Expr) bool {
	_, ok := e.(*ast.SelectorExpr)
	return ok
}

// globalRoot reports the package global an assignment target writes: the
// identifier itself (`repoDeps = …`) or the root of a field write
// (`collabDeps.ChecksCombined = …`), which mutates the same global.
func globalRoot(e ast.Expr) string {
	for {
		switch x := e.(type) {
		case *ast.Ident:
			for _, g := range packageGlobals {
				if x.Name == g {
					return g
				}
			}
			return ""
		case *ast.SelectorExpr:
			e = x.X
		case *ast.StarExpr:
			e = x.X
		case *ast.IndexExpr:
			e = x.X
		default:
			return ""
		}
	}
}
