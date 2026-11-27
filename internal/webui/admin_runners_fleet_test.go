package webui

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/threads"
)

func TestAdminRunnerFleetVerbs(t *testing.T) {
	s := newSettingsServer(t, &settingsFakes{deps: adminRunnerDeps(), principal: settingsTestPrincipal()})
	for _, verb := range []string{"drain", "resume", "deregister"} {
		rec := settingsDo(t, s, http.MethodPost, "/admin/runners/"+verb, url.Values{"id": {"abcd1234"}}, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200", verb, rec.Code)
		}
	}
	// Rotation renders the new credential in place, once.
	rec := settingsDo(t, s, http.MethodPost, "/admin/runners/rotate", url.Values{"id": {"abcd1234"}}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("rotate = %d", rec.Code)
	}
	body := rec.Body.String()
	settingsMustContain(t, body, "wrt_new.secret", "rotated token rendered once")
	settingsMustContain(t, body, "admin-runner-rotated-card", "rotated token card")
	if strings.Count(body, "wrt_new.secret") > 1 {
		t.Fatal("rotated token rendered more than once")
	}
	// A cross-origin POST and a missing id are refused.
	rec = settingsDo(t, s, http.MethodPost, "/admin/runners/drain", url.Values{"id": {"abcd1234"}},
		map[string]string{"Origin": "https://evil.example"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin = %d", rec.Code)
	}
	if rec := settingsDo(t, s, http.MethodPost, "/admin/runners/drain", url.Values{}, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing id = %d", rec.Code)
	}
	if rec := settingsDo(t, s, http.MethodGet, "/admin/runners/drain", nil, nil); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET drain = %d", rec.Code)
	}
}

func TestAdminRunnerFleetUnwiredAndErrors(t *testing.T) {
	// Unwired: the fleet verbs refuse with a named 503.
	deps := adminBaseDeps()
	s := newSettingsServer(t, &settingsFakes{deps: deps, principal: settingsTestPrincipal()})
	for _, verb := range []string{"drain", "resume", "rotate", "deregister"} {
		if rec := settingsDo(t, s, http.MethodPost, "/admin/runners/"+verb, url.Values{"id": {"abcd1234"}}, nil); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("unwired %s = %d", verb, rec.Code)
		}
	}
	// A failing verb renders the service's error page.
	deps = adminRunnerDeps()
	deps.RunnerTenancy.Rotate = func(context.Context, string, auth.Principal) (*threads.RunnerTokenRotation, error) {
		return nil, threads.ErrConflict
	}
	deps.RunnerTenancy.Drain = func(context.Context, string, auth.Principal) error { return threads.ErrConflict }
	deps.RunnerTenancy.Resume = func(context.Context, string, auth.Principal) error { return threads.ErrConflict }
	deps.RunnerTenancy.Deregister = func(context.Context, string, auth.Principal) error { return threads.ErrConflict }
	s = newSettingsServer(t, &settingsFakes{deps: deps, principal: settingsTestPrincipal()})
	for _, verb := range []string{"drain", "resume", "rotate", "deregister"} {
		if rec := settingsDo(t, s, http.MethodPost, "/admin/runners/"+verb, url.Values{"id": {"abcd1234"}}, nil); rec.Code != http.StatusConflict {
			t.Fatalf("%s conflict = %d", verb, rec.Code)
		}
	}
}
