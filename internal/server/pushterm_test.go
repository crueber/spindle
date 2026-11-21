package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/git"
)

// TestPushRefusalTerminatesForEitherReportCap is the rejected-pack hang
// regression: git requests ONLY `report-status-v2` when the server advertises
// it, so a refusal path that keys on `report-status` alone writes the band-2
// message and no report — and `git push` then blocks reading a status stream
// that never ends (the audit's "client sat 3+ minutes, twice"). Every refusal
// must terminate: band-2 message, then a report-status trailer with one
// `ng <ref>` line per command.
func TestPushRefusalTerminatesForEitherReportCap(t *testing.T) {
	cases := []struct {
		name       string
		caps       string
		wantReport bool
	}{
		{"report-status", "report-status side-band-64k", true},
		{"report-status-v2-only", "report-status-v2 side-band-64k", true},
		{"both", "report-status report-status-v2 side-band-64k", true},
		{"plain", "report-status", true},
		{"no-report-cap", "side-band-64k", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestServer(t, nil)
			fe := s.engine.(*fakeEngine)
			fe.exists = true
			ctx := context.WithValue(context.Background(), repoRootKey{}, t.TempDir())
			repo, err := fe.Repo(ctx, mustRepoID(t, "o/r"), true, git.Sha1)
			if err != nil {
				t.Fatal(err)
			}
			// Valid command line, unindexable pack → the ingest refusal path.
			body := pushBody(t, repo, strings.Repeat("1", 40), tc.caps,
				[]byte("PACK-not-a-real-pack"))
			req := httptest.NewRequest("POST", "http://x/o/r.git/git-receive-pack", bytes.NewReader(body))
			req.Header.Set("User-Agent", "git/2.53.0")
			req.Header.Set("Authorization", "Bearer tok123")
			req = req.WithContext(ctx)
			rec := httptest.NewRecorder()

			// A refusal that blocks is the bug; fail instead of hanging the
			// suite.
			done := make(chan struct{})
			go func() {
				defer close(done)
				s.receivePackLocal(rec, req, mustRepoID(t, "o/r"), principalAlice)
			}()
			select {
			case <-done:
			case <-time.After(60 * time.Second):
				t.Fatal("receive-pack refusal did not terminate within 60s")
			}

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body = %q", rec.Code, rec.Body.String())
			}
			out := rec.Body.String()
			if !strings.Contains(out, "pack rejected") {
				t.Fatalf("band-2 message missing: %q", out)
			}
			hasReport := strings.Contains(out, "unpack ng")
			hasRef := strings.Contains(out, "ng refs/heads/main")
			if tc.wantReport != hasReport {
				t.Fatalf("report trailer = %v, want %v (body %q)", hasReport, tc.wantReport, out)
			}
			if tc.wantReport && !hasRef {
				t.Fatalf("per-ref refusal missing (git reports %q): %q",
					"remote failed to report status", out)
			}
			if fe.published != 0 {
				t.Fatalf("rejected pack must not publish; publishes = %d", fe.published)
			}
		})
	}
}
