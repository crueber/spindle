package moderation

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/identity"
	"github.com/crueber/spindle/internal/server/auth"
)

// doBody runs one request with a JSON body through the handler.
func doBody(h *Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	if !h.Handle(rec, req) {
		rec.Code = http.StatusNotFound
	}
	return rec
}

func reportFileBody(kind string, num, seq int) string {
	b, _ := json.Marshal(ReportInput{Kind: kind, Target: ReportTargetRef{Num: num, Seq: seq}, Reason: "spam"})
	return string(b)
}

// principalSwitch is the one service + a mutable request principal, so a
// test can walk the role matrix without resetting the bucket.
type principalSwitch struct {
	h   *Handler
	svc *Service
	p   auth.Principal
}

func newSwitch(t *testing.T, role identity.Role) *principalSwitch {
	t.Helper()
	svc, _ := testService(t)
	svc.Roles = &fakeRoles{role: role}
	svc.Targets = fakeProbe{keys: map[string]bool{"thread": true, "review": true, "review_thread": true}}
	sw := &principalSwitch{svc: svc}
	sw.h = &Handler{Svc: svc, Auth: func(*http.Request) (auth.Principal, *auth.AuthError) {
		return sw.p, nil
	}}
	return sw
}

func (s *principalSwitch) as(p auth.Principal) *Handler { s.p = p; return s.h }

func TestReportsHTTPFloorsMatrix(t *testing.T) {
	ctx := ctx()
	sw := newSwitch(t, identity.RoleRead)
	svc := sw.svc
	// Anonymous: 401 on filing, listing and resolving.
	h := sw.as(auth.Anonymous())
	if rec := doBody(h, "POST", "/acme/widgets/api/moderation/reports", reportFileBody("thread", 12, 3)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon file = %d", rec.Code)
	}
	// A reader files: 201 with the report id and the card.
	h = sw.as(auth.Principal{Name: "bob"})
	rec := doBody(h, "POST", "/acme/widgets/api/moderation/reports", reportFileBody("thread", 12, 3))
	if rec.Code != http.StatusCreated {
		t.Fatalf("reader file = %d %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID     int        `json:"id"`
		Report ReportCard `json:"report"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID != 1 || created.Report.State != ReportOpen {
		t.Fatalf("created = %+v", created)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control = %q", cc)
	}
	// The same reporter + target is a 409; another target is fine.
	if rec := doBody(h, "POST", "/acme/widgets/api/moderation/reports", reportFileBody("thread", 12, 3)); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate = %d", rec.Code)
	}
	// The browser lane is the same handler.
	if rec := doBody(h, "POST", "/acme/widgets/api-browser/moderation/reports", reportFileBody("thread", 13, 1)); rec.Code != http.StatusCreated {
		t.Fatalf("browser lane file = %d %s", rec.Code, rec.Body.String())
	}
	// A reader cannot list or resolve (repo admin floor).
	if rec := do(h, "GET", "/acme/widgets/api/moderation/reports"); rec.Code != http.StatusForbidden {
		t.Fatalf("reader list = %d", rec.Code)
	}
	if rec := doBody(h, "POST", "/acme/widgets/api/moderation/reports/1/resolve", `{"action":"none"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("reader resolve = %d", rec.Code)
	}
	// The repo admin lists and resolves both verbs (same store; the role
	// gate reads the service's Roles seam).
	sw.svc.Roles = &fakeRoles{role: identity.RoleAdmin}
	h = sw.as(auth.Principal{Name: "admin"})
	rec = do(h, "GET", "/acme/widgets/api/moderation/reports")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"reports"`) {
		t.Fatalf("admin list = %d %s", rec.Code, rec.Body.String())
	}
	rec = doBody(h, "POST", "/acme/widgets/api/moderation/reports/1/resolve", `{"action":"hidden","note":"confirmed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin resolve = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"state":"resolved"`) || !strings.Contains(rec.Body.String(), `"action":"hidden"`) {
		t.Fatalf("resolved body = %s", rec.Body.String())
	}
	// Already terminal → 409.
	if rec := doBody(h, "POST", "/acme/widgets/api/moderation/reports/1/resolve", `{"action":"none"}`); rec.Code != http.StatusConflict {
		t.Fatalf("repeat resolve = %d", rec.Code)
	}
	// Dismiss accepts note only; an action field is a 400 (no shim).
	if rec := doBody(h, "POST", "/acme/widgets/api/moderation/reports/2/dismiss", `{"action":"hidden"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("dismiss with action = %d", rec.Code)
	}
	if rec := doBody(h, "POST", "/acme/widgets/api/moderation/reports/2/dismiss", `{"note":"not abuse"}`); rec.Code != http.StatusOK {
		t.Fatalf("dismiss = %d %s", rec.Code, rec.Body.String())
	}
	// The dismiss audit verb is report_dismiss (the plan's one spelling);
	// seq 1 is the hide record, seq 2 the resolve, seq 3 the dismissal.
	entry, err := svc.AuditEntryAt(ctx, 3)
	if err != nil || entry == nil || entry.Action != ActionReportDismiss {
		t.Fatalf("dismiss audit = %+v %v", entry, err)
	}
}

func TestReportsHTTPBadInputs(t *testing.T) {
	sw := newSwitch(t, identity.RoleRead)
	h := sw.as(auth.Principal{Name: "bob"})
	// Bad target grammar and unknown kind are 400.
	for _, body := range []string{
		`{"kind":"thread","target":{"num":0,"seq":1},"reason":"spam"}`,
		`{"kind":"bogus","target":{"num":1,"seq":1},"reason":"spam"}`,
		`{"kind":"thread","target":{"num":1,"seq":1},"reason":"boring"}`,
		`{"kind":"thread","target":{"num":1,"seq":1},"reason":"spam","extra":true}`,
		`not json`,
	} {
		if rec := doBody(h, "POST", "/acme/widgets/api/moderation/reports", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s = %d %s", body, rec.Code, rec.Body.String())
		}
	}
	// Bad report id on resolve.
	sw.svc.Roles = &fakeRoles{role: identity.RoleAdmin}
	h = sw.as(auth.Principal{Name: "admin"})
	if rec := doBody(h, "POST", "/acme/widgets/api/moderation/reports/x/resolve", `{"action":"none"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id = %d", rec.Code)
	}
	if rec := doBody(h, "POST", "/acme/widgets/api/moderation/reports/1/resolve", `{"action":"delete"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad action = %d", rec.Code)
	}
	// GET on the resolve path is not a route (405 or 404, never a write).
	if rec := do(h, "GET", "/acme/widgets/api/moderation/reports/1/resolve"); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET resolve = %d", rec.Code)
	}
}

func TestGlobalReportsAndAuditHTTP(t *testing.T) {
	// Seed one report with a repo admin, then read the queue as host admin.
	sw := newSwitch(t, identity.RoleRead)
	svc := sw.svc
	h := sw.as(auth.Principal{Name: "bob"})
	if rec := doBody(h, "POST", "/acme/widgets/api/moderation/reports", reportFileBody("thread", 12, 3)); rec.Code != http.StatusCreated {
		t.Fatalf("seed = %d %s", rec.Code, rec.Body.String())
	}
	admin := auth.Principal{Name: "root", Admin: true}
	sw.svc.Roles = &fakeRoles{role: identity.RoleAdmin}
	h = sw.as(admin)
	// Host admin reads the global queue (both lanes), no-store.
	for _, lane := range []string{"/api/v1", "/api-browser/v1"} {
		rec := do(h, "GET", lane+"/moderation/reports?state=open")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s queue = %d %s", lane, rec.Code, rec.Body.String())
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("cache-control = %q", cc)
		}
		var body struct {
			Reports []struct {
				Repo   string `json:"repo"`
				ID     int    `json:"id"`
				Cursor string `json:"cursor"`
			} `json:"reports"`
			More bool `json:"more"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body.Reports) != 1 || body.Reports[0].Repo != "acme/widgets" || body.Reports[0].ID != 1 || body.Reports[0].Cursor == "" {
			t.Fatalf("%s body = %s", lane, rec.Body.String())
		}
	}
	// Non-admin and anonymous are refused.
	h = sw.as(auth.Principal{Name: "bob"})
	if rec := do(h, "GET", "/api/v1/moderation/reports"); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin queue = %d", rec.Code)
	}
	h = sw.as(auth.Anonymous())
	if rec := do(h, "GET", "/api/v1/moderation/reports"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon queue = %d", rec.Code)
	}
	// The host-admin twin delegates to the repo writer: the report resolves
	// and the M2 hide is applied.
	h = sw.as(admin)
	rec := doBody(h, "POST", "/api/v1/moderation/reports/acme/widgets/1/resolve", `{"action":"hidden"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"repo":"acme/widgets"`) {
		t.Fatalf("host resolve = %d %s", rec.Code, rec.Body.String())
	}
	doc, err := svc.State(ctx(), "acme", "widgets")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Hidden[ThreadTarget(12, 3)]; !ok {
		t.Fatalf("host twin did not hide: %+v", doc.Hidden)
	}
	if rec := doBody(h, "POST", "/api-browser/v1/moderation/reports/bad", `{"action":"none"}`); rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("malformed global twin = %d", rec.Code)
	}
	// A non-admin cannot use the host twin.
	h = sw.as(auth.Principal{Name: "bob"})
	if rec := doBody(h, "POST", "/api/v1/moderation/reports/acme/widgets/1/resolve", `{"action":"none"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin twin = %d", rec.Code)
	}

	// Audit viewer: host admin only, entries + more, no-store.
	h = sw.as(admin)
	rec = do(h, "GET", "/api/v1/moderation/audit")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"entries"`) {
		t.Fatalf("audit = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"action":"report_resolve"`) {
		t.Fatalf("audit missing the resolve verb: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"action":"hide"`) {
		t.Fatalf("audit missing the M2 hide record: %s", rec.Body.String())
	}
	h = sw.as(auth.Principal{Name: "bob"})
	if rec := do(h, "GET", "/api/v1/moderation/audit"); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin audit = %d", rec.Code)
	}
	h = sw.as(auth.Anonymous())
	if rec := do(h, "GET", "/api/v1/moderation/audit"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon audit = %d", rec.Code)
	}
}
