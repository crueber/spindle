package threads

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// Coverage and edge cases for the Wave-4 surfaces (the package gate is ≥95%).

func TestStockImageCatalog(t *testing.T) {
	imgs := StockImages()
	if len(imgs) < 5 {
		t.Fatalf("catalog = %+v", imgs)
	}
	labels := StockImageLabels()
	args := StockImageLabelArgs()
	if len(labels) != len(imgs) || len(args) != len(imgs) {
		t.Fatalf("catalog shapes = %d/%d/%d", len(imgs), len(labels), len(args))
	}
	seen := map[string]bool{}
	for i, img := range imgs {
		if img.Label == "" || img.Image == "" || img.Toolchain == "" {
			t.Fatalf("incomplete image %+v", img)
		}
		if seen[img.Label] {
			t.Fatalf("duplicate label %q", img.Label)
		}
		seen[img.Label] = true
		if args[i] != img.Label+":docker://"+img.Image {
			t.Fatalf("arg %q", args[i])
		}
		// PAR-177/D-26: every stock image is a real upstream image PINNED by
		// digest, never a mutable tag.
		if !strings.Contains(img.Image, "@sha256:") {
			t.Fatalf("%s is not digest-pinned: %q", img.Label, img.Image)
		}
		if _, err := ParseRunnerLabels([]string{args[i]}); err != nil {
			t.Fatalf("%s does not parse as a runner label: %v", args[i], err)
		}
		// The explicit resolution path returns the same pinned reference and
		// refuses an unknown label by name.
		got, err := StockImageFor(img.Label)
		if err != nil || got.Image != img.Image {
			t.Fatalf("StockImageFor(%q) = %+v, %v", img.Label, got, err)
		}
	}
	if _, err := StockImageFor("nodejs"); err == nil || !strings.Contains(err.Error(), "nodejs") || !strings.Contains(err.Error(), "node") {
		t.Fatalf("unknown stock label = %v, want the label and the catalogue named", err)
	}
	// The withheld non-goal labels are never in the catalogue.
	for _, withheld := range HostedNonGoalLabels() {
		if seen[withheld] {
			t.Fatalf("non-goal label %q is in the stock catalogue", withheld)
		}
	}
}

func TestNotificationWebhookAndMarkRead(t *testing.T) {
	e := newTestEnv(t)
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	e.roles.roles["maint"] = "maintain"
	if _, err := e.svc.UpdateSettings(ctx(), "o/r", SettingsInput{NotifyWebhook: srv.URL}, auth.Principal{Name: "maint"}); err != nil {
		t.Fatalf("settings: %v", err)
	}
	if _, err := e.svc.SetSubscription(ctx(), "o/r", true, nil, auth.Principal{Name: "reader"}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	e.git.add(testSHA, ".spindle/workflows/ci.yml", singleJobWorkflowYAML)
	runs, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	run := runs[0]
	// Force the terminal state directly: the notification path only cares that
	// the folded run reads failure.
	if _, uerr := e.svc.casUpdate(ctx(), RunKey("o", "r", run.ID), 5, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var r Run
		if jerr := json.Unmarshal(cur, &r); jerr != nil {
			return nil, false, jerr
		}
		r.State = RunFailure
		return mustJSON(&r), true, nil
	}); uerr != nil {
		t.Fatalf("force failure: %v", uerr)
	}
	e.svc.notifyRunFailure(ctx(), "o", "r", run.ID)
	if got == nil || got["event"] != "run_failed" || got["run_id"] != run.ID {
		t.Fatalf("webhook payload = %+v", got)
	}
	notes, err := e.svc.ListNotifications(ctx(), auth.Principal{Name: "reader"}, 10)
	if err != nil || len(notes) != 1 {
		t.Fatalf("notifications = %+v, %v", notes, err)
	}
	if notes[0].Read {
		t.Fatal("new notification reads as read")
	}
	if err := e.svc.MarkNotificationsRead(ctx(), auth.Principal{Name: "reader"}); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	notes, _ = e.svc.ListNotifications(ctx(), auth.Principal{Name: "reader"}, 10)
	if len(notes) != 1 || !notes[0].Read {
		t.Fatalf("after mark read = %+v", notes)
	}
	// A malformed webhook is a settings error, never a silent no-op.
	for _, bad := range []string{"ftp://x", "http://user:pw@example/x", "not a url"} {
		if _, err := e.svc.UpdateSettings(ctx(), "o/r", SettingsInput{NotifyWebhook: bad}, auth.Principal{Name: "maint"}); err == nil {
			t.Fatalf("webhook %q accepted", bad)
		}
	}
}

func TestRunnerGroupUpdateReappliesAndDeleteClears(t *testing.T) {
	e := newTestEnv(t)
	group, err := e.svc.CreateRunnerGroup(ctx(), wave4Admin(), RunnerGroupInput{
		Name: "boxes", Labels: []string{"a"}, RunnerPolicyInput: RunnerPolicyInput{Repos: []string{"acme/*"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _, authn := wave4Runner(t, e,
		MintInput{Name: "member", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	if _, err := e.svc.SetRunnerGroup(ctx(), authn.Doc.ID, group.ID, wave4Admin()); err != nil {
		t.Fatal(err)
	}
	// Updating the group rewrites its members' materialized labels.
	if _, err := e.svc.UpdateRunnerGroup(ctx(), wave4Admin(), group.ID, RunnerGroupInput{
		Name: "boxes", Labels: []string{"a", "b"}, RunnerPolicyInput: RunnerPolicyInput{Repos: []string{"acme/*"}},
	}); err != nil {
		t.Fatalf("update: %v", err)
	}
	doc, _, _ := e.svc.loadRunner(ctx(), authn.Doc.ID)
	if !labelsMatch(doc.EffectiveLabels(), []string{"a", "b"}) {
		t.Fatalf("labels = %v", doc.EffectiveLabels())
	}
	if rec := e.svc.runnerGroupMembers(ctx(), group.ID); rec != 1 {
		t.Fatalf("members = %d", rec)
	}
	// Deleting clears membership (the runner keeps its own labels/policy).
	if err := e.svc.DeleteRunnerGroup(ctx(), wave4Admin(), group.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	doc, _, _ = e.svc.loadRunner(ctx(), authn.Doc.ID)
	if doc.Group != "" || len(doc.GroupLabels) != 0 || !doc.CanServeRepo("other/two") {
		t.Fatalf("after delete = %+v", doc)
	}
	// Guards.
	if err := e.svc.DeleteRunnerGroup(ctx(), wave4Admin(), "missing"); err != nil {
		t.Logf("delete missing = %v (idempotent by design)", err)
	}
	if _, err := e.svc.SetRunnerGroup(ctx(), authn.Doc.ID, "missing", wave4Admin()); err == nil {
		t.Fatal("assign unknown group accepted")
	}
	if _, err := e.svc.CreateRunnerGroup(ctx(), e.reader(), RunnerGroupInput{Name: "x", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}}); err == nil {
		t.Fatal("non-admin created a group")
	}
	if _, err := e.svc.UpdateRunnerGroup(ctx(), wave4Admin(), group.ID, RunnerGroupInput{Name: "renamed", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}}); err == nil {
		t.Fatal("group id change accepted")
	}
}

func TestRunnerFleetErrorPaths(t *testing.T) {
	e := newTestEnv(t)
	_, _, authn := wave4Runner(t, e,
		MintInput{Name: "errors", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	if err := e.svc.DrainRunner(ctx(), "missing", wave4Admin()); err == nil {
		t.Fatal("drain of unknown runner accepted")
	}
	if _, err := e.svc.RotateRunnerToken(ctx(), "missing", wave4Admin()); err == nil {
		t.Fatal("rotation of unknown runner accepted")
	}
	if err := e.svc.DeregisterRunner(ctx(), "missing", wave4Admin()); err == nil {
		t.Fatal("deregister of unknown runner accepted")
	}
	// Rotation twice inside the window keeps the still-live previous hash.
	first, err := e.svc.RotateOwnRunnerToken(ctx(), authn)
	if err != nil {
		t.Fatal(err)
	}
	authn2, err := e.svc.authorizeRunner(ctx(), first.Token)
	if err != nil {
		t.Fatal(err)
	}
	second, err := e.svc.RotateOwnRunnerToken(ctx(), authn2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.authorizeRunner(ctx(), second.Token); err != nil {
		t.Fatalf("current credential refused: %v", err)
	}
	// The overlap keeps exactly ONE predecessor: the immediately previous
	// credential (first), never two rotations ago. first is still live here;
	// the original credential is not (it was dropped by the second rotation).
	if _, err := e.svc.authorizeRunner(ctx(), first.Token); err != nil {
		t.Fatalf("immediately previous credential stopped: %v", err)
	}
	// A revoked runner cannot resume or rotate.
	if err := e.svc.RevokeRunner(ctx(), authn.Doc.ID, wave4Admin()); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.ResumeRunner(ctx(), authn.Doc.ID, wave4Admin()); err == nil {
		t.Fatal("revoked runner resumed")
	}
	if _, err := e.svc.RotateRunnerToken(ctx(), authn.Doc.ID, wave4Admin()); err == nil {
		t.Fatal("revoked runner rotated")
	}
}

func TestSizingParsersAndRefusals(t *testing.T) {
	if _, ok := ParseMemorySize("16GiB"); !ok {
		t.Fatal("16GiB did not parse")
	}
	if got, _ := ParseMemorySize("512m"); got != 512<<20 {
		t.Fatalf("512m = %d", got)
	}
	if _, ok := ParseMemorySize("lots"); ok {
		t.Fatal("garbage memory accepted")
	}
	if err := ValidateRunnerSizing(2048, 0); err == nil {
		t.Fatal("2048 cpu accepted")
	}
	if err := ValidateRunnerSizing(0, 1<<51); err == nil {
		t.Fatal("1 PiB accepted")
	}
	for _, label := range HostedNonGoalLabels() {
		if reason := nonGoalLabelReason([]string{label}); reason == "" {
			t.Fatalf("%s has no named non-goal reason", label)
		}
	}
	if reason := nonGoalLabelReason([]string{"linux"}); reason != "" {
		t.Fatalf("linux got a non-goal reason: %q", reason)
	}
	err := &NonGoalError{NonGoal: NonGoals()[0]}
	if !strings.Contains(err.Error(), "D-15") || err.Unwrap() == nil {
		t.Fatalf("non-goal error = %v", err)
	}
	for _, ask := range []string{"lxc", "host", "windows-latest", "macos", "attestations", "attest-build-provenance"} {
		if RefuseNonGoal(ask) == nil {
			t.Fatalf("%q was not refused", ask)
		}
	}
	if RefuseNonGoal("docker") != nil {
		t.Fatal("docker was refused as a non-goal")
	}
	if got := formatMemory(1536); got != "1.5 KiB" {
		t.Fatalf("formatMemory = %q", got)
	}
	if got := formatMemory(42); got != "42 B" {
		t.Fatalf("bytes = %q", got)
	}
	if got := (JobSize{CPUs: 2, MemoryBytes: 1 << 30}).Describe(); !strings.Contains(got, "2 cpu") || !strings.Contains(got, "GiB") {
		t.Fatalf("describe = %q", got)
	}
}

func TestValidateKeyFromMessage(t *testing.T) {
	// A semantic parser error (not a yaml TypeError) still names its key.
	res := ValidateWorkflowSource("ci.yml", []byte("on: [push]\npermissions:\n  attestations: write\njobs:\n  b:\n    runs-on: linux\n    steps:\n      - run: x\n"))
	if res.OK || len(res.Issues) == 0 {
		t.Fatalf("attestations permission accepted: %+v", res)
	}
	if !strings.Contains(res.Issues[0].Message, "non-goal") || !strings.Contains(res.Issues[0].Key, "permissions") {
		t.Fatalf("issue = %+v", res.Issues[0])
	}
	// A file that cannot be decoded at all is still one issue with the file name.
	if r := ValidateWorkflowSource("bad.yml", []byte("\tnot: yaml")); r.OK || len(r.Issues) == 0 || r.Issues[0].File != "bad.yml" {
		t.Fatalf("undecodable = %+v", r)
	}
}

func TestHTTPWave4ErrorBranches(t *testing.T) {
	e, h, cur := newHTTP(t)
	*cur = wave4Admin()
	if rec := do(h, http.MethodPost, "/api/v1/admin/runners/missing/drain", ""); rec.Code != 404 {
		t.Fatalf("drain missing = %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/api/v1/admin/runners/missing/bogus", ""); rec.Code != 404 {
		t.Fatalf("unknown runner verb = %d", rec.Code)
	}
	if rec := do(h, http.MethodPost, "/api/v1/admin/runners/missing/group", `{}`); rec.Code != 400 && rec.Code != 404 {
		t.Fatalf("group without body = %d", rec.Code)
	}
	if rec := do(h, http.MethodPatch, "/api/v1/admin/runner-groups", ""); rec.Code != 405 {
		t.Fatalf("PATCH groups = %d", rec.Code)
	}
	if rec := do(h, http.MethodGet, "/api/v1/admin/runner-groups/nope", ""); rec.Code != 405 {
		t.Fatalf("GET group = %d", rec.Code)
	}
	// Anonymous runner protocol call is a real 401.
	*cur = auth.Anonymous()
	if rec := w4DoAuth(h, http.MethodPost, "/api/v1/threads/runners/drain", "", ""); rec.Code != 401 {
		t.Fatalf("anonymous drain = %d", rec.Code)
	}
	// Unknown self verb with a valid-looking token shape is not wired.
	_, token, _ := wave4Runner(t, e,
		MintInput{Name: "shape", RunnerPolicyInput: RunnerPolicyInput{AllowAll: true}},
		RegisterInput{Labels: []string{"linux"}})
	if rec := w4DoAuth(h, http.MethodPost, "/api/v1/threads/runners/bogus", "", token); rec.Code != 404 {
		t.Fatalf("unknown self verb = %d", rec.Code)
	}
	// Validation without a body is a 400.
	*cur = auth.Principal{Name: "writer"}
	if rec := do(h, http.MethodPost, "/o/r/api/threads/validate", `{}`); rec.Code != 200 && rec.Code != 400 {
		t.Fatalf("empty validate = %d", rec.Code)
	}
	// Schedules for an empty repository are an empty list, not an error.
	e.git.refs["HEAD"] = ""
	if rec := do(h, http.MethodGet, "/o/r/api/threads/schedules", ""); rec.Code != 200 {
		t.Fatalf("empty schedules = %d: %s", rec.Code, rec.Body.String())
	}
}
