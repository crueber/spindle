package webui

// admin_gc_test.go — the Maintenance page's packages-GC control (PLAN-ADMIN
// §8.6): the dry-run copy, the confirmed apply, the last-run facts, the live
// task card, the not-wired and restricted states, and the server-side
// confirmation rule. The pass itself is tested in internal/packages; the
// stream plumbing is the API's shared pump.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/api"
	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/packages"
	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/wal"
)

// gcDeps is a Maintenance page whose GC seam records every start.
type gcStarts struct {
	calls []string // "<actor>|dry=<bool>"
	task  *AdminGCTask
	err   error
}

func gcStatusDeps(st *gcStarts, status packages.GCStatus) testDeps {
	deps := adminBaseDeps()
	deps.PackagesGCStatus = func(context.Context) (packages.GCStatus, error) { return status, nil }
	deps.PackagesGCStart = func(_ context.Context, actor string, dryRun bool) (*AdminGCTask, error) {
		st.calls = append(st.calls, actor+"|dry="+boolWord(dryRun))
		if st.err != nil {
			return nil, st.err
		}
		if st.task != nil {
			return st.task, nil
		}
		return &AdminGCTask{ID: "task-1", Kind: packages.TaskKindGC, DryRun: dryRun}, nil
	}
	deps.PackagesGCAttach = func(context.Context, string) (api.TaskStream, bool, error) {
		return api.TaskStream{}, true, nil
	}
	return deps
}

func boolWord(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestAdminMaintenanceGCDryRunCopy(t *testing.T) {
	st := &gcStarts{}
	body := settingsDo(t, maintenanceServer(t, gcStatusDeps(st, packages.GCStatus{}), nil),
		http.MethodGet, "/admin/maintenance", nil, nil).Body.String()

	for _, want := range []string{
		"Packages garbage collection",
		"Dry run — count, do not delete",
		"Run GC now",
		"counts", // the dry run's copy says it counts
		"not a promise",
		"packages/gc_state.json",
		"15m", // the per-repo due gate, surfaced not duplicated
		"10m", // the blob grace
		"The maintainer sweeps each repository",
	} {
		settingsMustContain(t, body, want, "packages GC section")
	}
	// Exactly one primary: the confirmed apply. The dry run is a plain .btn.
	if got := strings.Count(body, "btn primary"); got != 1 {
		t.Fatalf("primary buttons = %d, want 1 (the apply)", got)
	}
	// The apply form carries the typed confirmation (the danger-zone idiom).
	settingsMustContain(t, body, `data-danger-expected="apply"`, "apply confirmation")
	settingsMustContain(t, body, `data-danger-arm`, "the armed confirmation input")
	settingsMustContain(t, body, `name="op" value="dry-run"`, "dry-run op")
	settingsMustContain(t, body, `name="op" value="apply"`, "apply op")
}

func TestAdminMaintenanceGCLastRuns(t *testing.T) {
	status := packages.GCStatus{State: &packages.GCState{
		Version: 1,
		Last: &packages.GCRun{
			At: "2026-11-20T10:00:00Z", DryRun: true, Actor: "alice", Repos: 3,
			Deleted: 4, Bytes: 2048, Uploads: 1, OK: true,
		},
		LastApply: &packages.GCRun{
			At: "2026-11-19T10:00:00Z", Actor: "bob", Repos: 2, Deleted: 7,
			Bytes: 4096 * 1024, OK: false, Error: "one registry unreadable", TaskID: "t-9",
		},
	}}
	body := settingsDo(t, maintenanceServer(t, gcStatusDeps(&gcStarts{}, status), nil),
		http.MethodGet, "/admin/maintenance", nil, nil).Body.String()

	for _, want := range []string{
		"apply", "dry run", "bob", "alice",
		"reclaimed 4.0 MiB", "would reclaim 2.0 KiB",
		"failed", "one registry unreadable", "chip-open",
	} {
		settingsMustContain(t, body, want, "last-run facts")
	}
}

func TestAdminMaintenanceGCRunningCard(t *testing.T) {
	status := packages.GCStatus{
		Running: &wal.TaskRecord{
			ID: "live-7", Kind: packages.TaskKindGC, Repo: packages.InstanceScope,
			Started: "2026-11-20T11:00:00Z", Params: map[string]string{"dry_run": "1"},
		},
		State: &packages.GCState{Version: 1},
	}
	s := maintenanceServer(t, gcStatusDeps(&gcStarts{}, status), nil)
	body := settingsDo(t, s, http.MethodGet, "/admin/maintenance", nil, nil).Body.String()
	settingsMustContain(t, body, "running", "running chip")
	settingsMustContain(t, body, "A pass is running now", "running copy")
	settingsMustContain(t, body, "counting only", "dry-run running copy")
}

func TestAdminMaintenanceGCNotWired(t *testing.T) {
	deps := adminBaseDeps() // no GC seams
	body := settingsDo(t, maintenanceServer(t, deps, nil), http.MethodGet, "/admin/maintenance", nil, nil).Body.String()
	settingsMustContain(t, body, "Packages garbage collection", "the section still renders")
	settingsMustContain(t, body, "not wired", "honest not-wired state")
	settingsMustNotContain(t, body, `name="op" value="apply"`, "no control without a seam")
}

func TestAdminMaintenanceGCRestricted(t *testing.T) {
	deps := gcStatusDeps(&gcStarts{}, packages.GCStatus{})
	deps.ConfigView = func(context.Context) (*AdminConfigView, error) {
		v := adminFakeView()
		v.FileState = "invalid"
		return v, nil
	}
	cfg := config.Defaults()
	cfg.Cache.Dir = t.TempDir()
	s := maintenanceServer(t, deps, cfg)
	body := settingsDo(t, s, http.MethodGet, "/admin/maintenance", nil, nil).Body.String()
	settingsMustContain(t, body, "Restricted mode has no object store, so no registry exists", "restricted copy")
	settingsMustNotContain(t, body, `name="op" value="apply"`, "no GC control in restricted mode")
}

// TestAdminGCStartDryRunAndApply covers the POST: a dry run starts at once, an
// apply needs the confirmation tick, and the redirect carries the task id.
func TestAdminGCStartDryRunAndApply(t *testing.T) {
	st := &gcStarts{}
	s := maintenanceServer(t, gcStatusDeps(st, packages.GCStatus{}), nil)

	rec := settingsDo(t, s, http.MethodPost, "/admin/maintenance/gc", url.Values{"op": {"dry-run"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("dry run = %d %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "gc=task-1") || !strings.Contains(loc, "gc_dry=1") {
		t.Fatalf("dry-run redirect = %q", loc)
	}
	if len(st.calls) != 1 || !strings.Contains(st.calls[0], "dry=true") || !strings.HasPrefix(st.calls[0], "alice|") {
		t.Fatalf("starts = %v", st.calls)
	}

	// Apply without the tick: a named error, no start, no 500.
	rec = settingsDo(t, s, http.MethodPost, "/admin/maintenance/gc", url.Values{"op": {"apply"}}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unconfirmed apply = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "confirm the apply pass first", "confirmation error")
	if len(st.calls) != 1 {
		t.Fatalf("an unconfirmed apply started a pass: %v", st.calls)
	}

	// Confirmed: starts and redirects without the dry flag.
	rec = settingsDo(t, s, http.MethodPost, "/admin/maintenance/gc", url.Values{"op": {"apply"}, "confirm": {"apply"}}, nil)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("apply = %d %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "gc_dry") {
		t.Fatalf("apply redirect = %q", loc)
	}
	if len(st.calls) != 2 || !strings.Contains(st.calls[1], "dry=false") {
		t.Fatalf("starts = %v", st.calls)
	}

	// The card renders for the redirected-to task.
	body := settingsDo(t, s, http.MethodGet, "/admin/maintenance?gc=task-1", nil, nil).Body.String()
	settingsMustContain(t, body, "data-task-id=\"task-1\"", "task card id")
	settingsMustContain(t, body, "/admin/maintenance/gc/stream?id=task-1", "task stream URL")
	settingsMustContain(t, body, "data-task-log", "log slot")
}

func TestAdminGCStartUnknownOpAndStartError(t *testing.T) {
	st := &gcStarts{}
	s := maintenanceServer(t, gcStatusDeps(st, packages.GCStatus{}), nil)
	rec := settingsDo(t, s, http.MethodPost, "/admin/maintenance/gc", url.Values{"op": {"nonsense"}}, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown op = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "choose a dry run or an apply pass", "op error")

	st2 := &gcStarts{err: errors.New("task table down")}
	s2 := maintenanceServer(t, gcStatusDeps(st2, packages.GCStatus{}), nil)
	rec = settingsDo(t, s2, http.MethodPost, "/admin/maintenance/gc", url.Values{"op": {"dry-run"}}, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("start error = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "task table down", "named start error")
}

// TestAdminGCStartUnwiredAndUnauthorized covers the two other refusals: a build
// without the seam, and a non-admin (the area's one gate, never a 200).
func TestAdminGCStartUnwiredAndUnauthorized(t *testing.T) {
	s := maintenanceServer(t, adminBaseDeps(), nil)
	rec := settingsDo(t, s, http.MethodPost, "/admin/maintenance/gc", url.Values{"op": {"dry-run"}}, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unwired = %d", rec.Code)
	}
	settingsMustContain(t, rec.Body.String(), "not wired", "unwired error")

	// A signed-in non-admin is refused by the area's gate with the API's 403.
	deps := adminBaseDeps()
	s2 := adminTestPage(t, deps, auth.Principal{Name: "bob", Email: "bob@example.com", Write: true})
	rec = settingsDo(t, s2, http.MethodPost, "/admin/maintenance/gc", url.Values{"op": {"dry-run"}}, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin = %d, want 403", rec.Code)
	}
}

// TestAdminGCStreamGuards pins the stream's refusals: no id, unknown task.
func TestAdminGCStreamGuards(t *testing.T) {
	deps := gcStatusDeps(&gcStarts{}, packages.GCStatus{})
	s := maintenanceServer(t, deps, nil)
	rec := settingsDo(t, s, http.MethodGet, "/admin/maintenance/gc/stream", nil, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no id = %d", rec.Code)
	}
	deps.PackagesGCAttach = func(context.Context, string) (api.TaskStream, bool, error) {
		return api.TaskStream{}, false, nil
	}
	s2 := maintenanceServer(t, deps, nil)
	rec = settingsDo(t, s2, http.MethodGet, "/admin/maintenance/gc/stream?id=nope", nil, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown task = %d", rec.Code)
	}
}
