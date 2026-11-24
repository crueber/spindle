package webui

// testsupport_test.go — test-only construction helpers.
//
// # Why this file exists
//
// Production parses the page set once per process (server.go: "One instance
// per process"), so New() legitimately pays for ~57 page sets x (base + every
// partial + the page). The test suite calls New() once per fixture — 800+
// times across 59 files — and under -race that parse was >90% of the
// package's wall time (624 s measured on a 10-core host, 543 s on the loaded
// runner that hit the CI ceiling): a render test spent seconds re-parsing
// bytes that cannot change.
//
// The embedded FS (render.go tplFS) and the template FuncMap (helpers.go
// funcs, which closes over nothing) are server-independent, and a parsed
// *template.Template is safe for concurrent Execute, so the tests parse the
// full set once and share it (each server gets its own map over the shared
// sets, so a test cannot swap a page under a peer). Sharing is exactly the
// production shape; the first caller still runs the real New(), so the parse
// path — and a boot-time template error — stays covered by the suite.
//
// # The parallelism rule
//
// Package-wide seams (BindAdmin, BindSettings, BindCollab, BindForms, …) are
// process globals by design, so a test that installs one is NOT independent:
// handlers read the bound value while the test runs. Top-level tests may call
// t.Parallel() only when they bind nothing and mutate no package-level
// variable (TestParallelTestsBindNoGlobals enforces this); the binding tests
// stay sequential and therefore always finish before Go resumes the parallel
// batch (the testing package runs every parallel test only after the last
// sequential one returns). Two product globals are saved/restored by tests
// (adminRestartDelay, assetUploadBodyLimit); those tests are sequential for
// the same reason.

import (
	"html/template"
	"sync"
	"testing"
)

// fullTemplates is the parsed full page set shared by every test server.
var fullTemplates struct {
	once sync.Once
	tpl  map[string]*template.Template
	err  error
}

// newTestServer is New() with the page set parsed once per test binary.
func newTestServer(t *testing.T, env Env) *Server {
	t.Helper()
	fullTemplates.once.Do(func() {
		s, err := New(env)
		if err != nil {
			fullTemplates.err = err
			return
		}
		fullTemplates.tpl = s.tpl
	})
	if fullTemplates.err != nil {
		t.Fatalf("webui.New: %v", fullTemplates.err)
	}
	s := &Server{env: env, tpl: cloneTplSet(fullTemplates.tpl)}
	s.mount()
	return s
}

// cloneTplSet gives each server its own page map, so a test that swaps one
// page on its own server cannot affect a peer. The *template sets* are shared
// and read-only (a parsed html/template is safe for concurrent Execute).
func cloneTplSet(m map[string]*template.Template) map[string]*template.Template {
	out := make(map[string]*template.Template, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// onceTemplates memoizes one group harness's parsed template set. The group
// harnesses (settings, forms) parse a SUBSET of the tree so a sibling group's
// half-written file cannot make them red; the subset is fixed, so it is
// parsed once too.
type onceTemplates struct {
	once sync.Once
	tpl  map[string]*template.Template
	err  error
}

// get runs build once and returns a per-caller copy of the shared map.
func (o *onceTemplates) get(t *testing.T, build func() (map[string]*template.Template, error)) map[string]*template.Template {
	t.Helper()
	o.once.Do(func() { o.tpl, o.err = build() })
	if o.err != nil {
		t.Fatalf("webui: parse templates: %v", o.err)
	}
	return cloneTplSet(o.tpl)
}
