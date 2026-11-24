package threads

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/server/auth"
	"github.com/crueber/spindle/internal/store"
)

// Error paths and small helpers (the protocol contract lives in
// protocol_test.go; this file keeps the per-package coverage gate honest).

func TestTokenHelpers(t *testing.T) {
	for _, tc := range []struct{ tok, prefix string }{
		{"", regTokenPrefix},
		{"wrg_only", regTokenPrefix},
		{"wrg_a.b", regTokenPrefix},
		{"wrg_!!!.abcdefgh.e", regTokenPrefix},
		{"wrg_YQ.short.e", regTokenPrefix},
		{"wrg_YQ.abcdefgh.0000", regTokenPrefix},
	} {
		if _, _, _, ok := tokenParts(tc.tok, tc.prefix); ok {
			t.Fatalf("tokenParts(%q) must fail", tc.tok)
		}
	}
	if _, _, _, ok := tokenParts("wrt_YQ.abcdefgh1234.0123456789abcdef", runnerTokenPrefix); !ok {
		t.Fatal("a well-formed token must parse")
	}
	if verifySecret("a", "bb") {
		t.Fatal("a length mismatch must not verify")
	}
	if verifySecret("a", hashSecret("b")) {
		t.Fatal("a wrong secret must not verify")
	}
	if !verifySecret("a", hashSecret("a")) {
		t.Fatal("the stored hash must verify")
	}
	if bearerToken("") != "" || bearerToken("basic x") != "" || bearerToken("Bearer") != "" {
		t.Fatal("bearerToken must reject non-bearer headers")
	}
	if bearerToken("bearer  abc ") != "abc" {
		t.Fatal("bearerToken must accept case-insensitive schemes")
	}
	for _, bad := range []string{"", "0", "-1", "x", "1234567890123"} {
		if _, err := attemptPathNumber(bad); err == nil {
			t.Fatalf("attemptPathNumber(%q) must fail", bad)
		}
	}
	if n, err := attemptPathNumber(" 7 "); err != nil || n != 7 {
		t.Fatalf("attemptPathNumber(7) = %d, %v", n, err)
	}
	if ttl := attemptTokenTTL(nil); ttl != 2*time.Hour {
		t.Fatalf("default ttl = %v", ttl)
	}
	if ttl := attemptTokenTTL(&Spec{TimeoutMinutes: 600}); ttl != 6*time.Hour {
		t.Fatalf("capped ttl = %v", ttl)
	}
	if ttl := attemptTokenTTL(&Spec{TimeoutMinutes: 15}); ttl != 25*time.Minute {
		t.Fatalf("job ttl = %v", ttl)
	}
	// Keyrings.
	k, err := NewKeyring("", "")
	if err != nil || !k.Ephemeral || k.SecretsEnabled() || len(k.Sign) != 32 {
		t.Fatalf("ephemeral keyring = %+v, %v", k, err)
	}
	k2, err := NewKeyring(strings.Repeat("a", 32), strings.Repeat("b", 32))
	if err != nil || k2.Ephemeral || !k2.SecretsEnabled() {
		t.Fatalf("configured keyring = %+v, %v", k2, err)
	}
	a, _ := NewKeyring(strings.Repeat("a", 32), "")
	b, _ := NewKeyring(strings.Repeat("b", 32), "")
	if string(a.Sign) == string(b.Sign) || string(a.Seal) == string(b.Seal) {
		t.Fatal("different secrets must derive different keys")
	}
	if string(a.Seal) == string(a.Sign) {
		t.Fatal("seal and sign keys must be purpose-separated")
	}
	if (&Keyring{}).SecretsEnabled() {
		t.Fatal("an empty keyring must not enable secrets")
	}
	var nilKeyring *Keyring
	if nilKeyring.SecretsEnabled() {
		t.Fatal("nil keyring")
	}
}

func TestJobRefRoundTrip(t *testing.T) {
	run := "1795176002000000000-abcdef"
	ref := JobRef("o/r", run, "j-build")
	owner, repo, gotRun, job, err := parseJobRef(ref)
	if err != nil || owner != "o" || repo != "r" || gotRun != run || job != "j-build" {
		t.Fatalf("parseJobRef = %q %q %q %q, %v", owner, repo, gotRun, job, err)
	}
	// A malformed repository still yields a parseable (but rejected) ref.
	if got := JobRef("bad repo", run, "j-build"); !strings.HasPrefix(got, jobRefPrefix) {
		t.Fatalf("JobRef(bad) = %q", got)
	}
	for _, bad := range []string{
		"", "jr_", "jr_!!!", "jr_" + b64("o/r/"+run), "jr_" + b64("o/r/x/j-build"),
		"jr_" + b64("o/r/"+run+"/bad job"), "xx" + b64("o/r/"+run+"/j-build"),
	} {
		if _, _, _, _, err := parseJobRef(bad); err == nil {
			t.Fatalf("parseJobRef(%q) must fail", bad)
		}
	}
	if padInt(5, 3) != "005" || padInt(1234, 3) != "1234" {
		t.Fatal("padInt")
	}
	if logFrameName(2, 7) != "002-0000000000000000007" {
		t.Fatalf("logFrameName = %q", logFrameName(2, 7))
	}
}

func b64(s string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	var out strings.Builder
	for _, b := range []byte(s) {
		out.WriteByte(alphabet[int(b)%64])
	}
	return out.String()
}

func TestAttemptTokenParsing(t *testing.T) {
	e := newTestEnv(t)
	claims := AttemptClaims{
		Repo: "o/r", Run: "1795176002000000000-abcdef", Job: "j-build", Attempt: 1,
		Runner: "rnr", Scopes: []string{ScopeLog}, Exp: e.now.Add(time.Hour).Unix(),
	}
	tok, _, err := e.svc.mintAttemptToken(claims)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	got, err := e.svc.parseAttemptToken(tok, e.now)
	if err != nil || got.Attempt != 1 || !got.Allows(ScopeLog) || got.Allows(ScopeComplete) {
		t.Fatalf("parse = %+v, %v", got, err)
	}
	for _, bad := range []string{"", "wrt_x.y.z", "wjt_", "wjt_a", "wjt_!!!.###"} {
		if _, err := e.svc.parseAttemptToken(bad, e.now); err == nil {
			t.Fatalf("parseAttemptToken(%q) must fail", bad)
		}
	}
	// A token with no keyring fails closed.
	plain := &Service{}
	if _, _, err := plain.mintAttemptToken(claims); err == nil {
		t.Fatal("mint without a keyring must fail")
	}
	if _, err := plain.parseAttemptToken(tok, e.now); err == nil {
		t.Fatal("parse without a keyring must fail")
	}
}

func TestQueueHelpers(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	jobs, err := e.svc.loadJobs(ctx(), "o", "r", run.ID)
	if err != nil || len(jobs) != 2 {
		t.Fatalf("jobs = %+v, %v", jobs, err)
	}
	// enqueueJob twice with the same job creates two entries (sequences differ).
	entry, err := e.svc.enqueueJob(ctx(), "o", "r", jobs[0], nil)
	if err != nil || entry.Seq == "" {
		t.Fatalf("enqueueJob = %+v, %v", entry, err)
	}
	// appendHead is idempotent for an already-listed seq.
	if err := e.svc.appendHead(ctx(), "o", "r", entry.Seq); err != nil {
		t.Fatalf("appendHead duplicate: %v", err)
	}
	head, _, err := e.svc.loadHead(ctx(), "o", "r")
	if err != nil || head == nil {
		t.Fatalf("head = %+v, %v", head, err)
	}
	if len(head.Pending) != 3 {
		t.Fatalf("pending = %v", head.Pending)
	}
	// trimHead is a no-op when nothing is dropped, and drops on request.
	e.svc.trimHead(ctx(), "o", "r", nil)
	e.svc.trimHead(ctx(), "o", "r", map[string]bool{entry.Seq: true})
	if head, _, _ := e.svc.loadHead(ctx(), "o", "r"); len(head.Pending) != 2 {
		t.Fatalf("pending after trim = %v", head.Pending)
	}
	// Corrupt records fail closed.
	if _, err := store.PutBytes(ctx(), e.store, QueueEntryKey("o", "r", "bad"), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, err := e.svc.loadEntry(ctx(), "o", "r", "bad"); err == nil {
		t.Fatal("a corrupt entry must fail")
	}
	if _, err := store.PutBytes(ctx(), e.store, QueueHeadKey("o", "r"), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed head: %v", err)
	}
	if _, _, err := e.svc.loadHead(ctx(), "o", "r"); err == nil {
		t.Fatal("a corrupt head must fail")
	}
	// leaseFor floors and caps.
	if got := e.svc.leaseFor(&Job{}); got != 15*time.Minute {
		t.Fatalf("lease floor = %v", got)
	}
	if got := e.svc.leaseFor(&Job{Spec: &Spec{TimeoutMinutes: 600}}); got != 30*time.Minute {
		t.Fatalf("lease cap = %v", got)
	}
	// claimExpired.
	if !e.svc.claimExpired(&QueueEntry{}) {
		t.Fatal("a claim with no lease is expired")
	}
	if !e.svc.claimExpired(&QueueEntry{LeaseUntil: ptr("not-a-stamp")}) {
		t.Fatal("a bad lease stamp is expired")
	}
	future := e.now.Add(time.Hour).Format(dateTimeFmt)
	if e.svc.claimExpired(&QueueEntry{LeaseUntil: &future}) {
		t.Fatal("a live lease is not expired")
	}
	// setEntryCancel on an absent entry is a no-op.
	e.svc.setEntryCancel(ctx(), "o", "r", "missing", true)
	e.svc.finishEntry(ctx(), "o", "r", "")
	// The hub wakes and unsubscribes cleanly.
	ch, cancel := fetchHub.subscribe("o/r")
	fetchHub.wake("o/r")
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("wake did not deliver")
	}
	cancel()
	cancel() // idempotent
	// Labels.
	if got := normalizeLabels([]string{" Linux ", "linux", "", "GPU"}); len(got) != 2 || got[0] != "gpu" || got[1] != "linux" {
		t.Fatalf("normalizeLabels = %v", got)
	}
	if !labelsMatch([]string{"a"}, nil) || labelsMatch(nil, []string{"a"}) || !labelsMatch([]string{"a", "b"}, []string{"b"}) {
		t.Fatal("labelsMatch")
	}
	if got := jobLabels(&Job{RunsOn: "linux, x64"}); len(got) != 2 {
		t.Fatalf("jobLabels fallback = %v", got)
	}
}

func TestQueueMidClaimMovesOn(t *testing.T) {
	e := newTestEnv(t)
	e.useIndependentJobs() // the subject is entry retirement, not ordering
	run := e.mustCreate(t)[0]
	// Cancel the first job by hand: a queued entry whose job is cancelled is
	// retired by the probe instead of being offered.
	jobs, _ := e.svc.loadJobs(ctx(), "o", "r", run.ID)
	if _, err := e.svc.UpdateJob(ctx(), "o/r", run.ID, jobs[0].ID, JobUpdate{State: JobCancelled}); err != nil {
		t.Fatalf("cancel job: %v", err)
	}
	runner := &runnerAuth{Doc: &RunnerDoc{ID: "rnr", Labels: []string{"linux"}, AllowAll: true, State: RunnerActive}}
	claim, err := e.svc.claimLoop(ctx(), "o", "r", runner.Doc, runner.Doc.Labels)
	if err != nil || claim == nil {
		t.Fatalf("claimLoop = %+v, %v", claim, err)
	}
	if claim.Entry.JobID == jobs[0].ID {
		t.Fatalf("a cancelled job must not be offered: %+v", claim.Entry)
	}
}

func TestServiceQueueWritesOnCorruptHead(t *testing.T) {
	e := newTestEnv(t)
	if _, err := store.PutBytes(ctx(), e.store, QueueHeadKey("o", "r"), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.CreateRun(ctx(), "o/r", testSHA, EventPush, CreateOpts{Branch: "main"}); err != nil {
		// Run creation must never fail because the queue head is unreadable:
		// the job stays queued (the pre-runner behaviour).
		t.Fatalf("CreateRun with a corrupt head = %v", err)
	}
}

func TestRunnerRegistryEdges(t *testing.T) {
	e, h, cur := newHTTP(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	*cur = admin
	// Anonymous mint is refused.
	if _, err := e.svc.MintRegistrationToken(ctx(), auth.Anonymous(), mintAllPolicy("")); err == nil {
		t.Fatal("an anonymous registration token must be refused")
	}
	// Unknown runner mutations.
	if rec := doAuth(h, http.MethodDelete, "/api/v1/admin/runners/abcdefgh1234", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown revoke = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodDelete, "/api/v1/admin/runners/bogus!", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("bad id revoke = %d", rec.Code)
	}
	// Corrupt registry/credential records fail closed.
	mint, err := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy(""))
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := store.PutBytes(ctx(), e.store, RunnerKey(mint.ID), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if rec := doAuth(h, http.MethodDelete, "/api/v1/admin/runners/"+mint.ID, "", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("corrupt registry revoke = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodGet, "/api/v1/admin/runners", "", ""); rec.Code != http.StatusInternalServerError {
		t.Fatalf("corrupt registry list = %d", rec.Code)
	}
	// Register with a malformed, unknown and expired token.
	for _, body := range []string{
		`{"token":"nope"}`,
		`{"token":"wrg_YQ_abcdefgh1234.0123456789abcdef"}`, // unknown principal
	} {
		if rec := doAuth(h, http.MethodPost, "/api/v1/threads/runners/register", body, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("register %s = %d", body, rec.Code)
		}
	}
	// An expiring registration token is refused.
	mint2, err := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy(""))
	if err != nil {
		t.Fatalf("mint2: %v", err)
	}
	e.advance(RegistrationTTL + time.Minute)
	body := `{"token":"` + mint2.Token + `","labels":["linux"]}`
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/runners/register", body, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired registration = %d", rec.Code)
	}
	// A revoked credential is refused by declare.
	mint3, err := e.svc.MintRegistrationToken(ctx(), admin, mintAllPolicy(""))
	if err != nil {
		t.Fatalf("mint3: %v", err)
	}
	reg := doAuth(h, http.MethodPost, "/api/v1/threads/runners/register",
		`{"token":"`+mint3.Token+`"}`, "")
	var registered RegisterResult
	if err := json.Unmarshal(reg.Body.Bytes(), &registered); err != nil {
		t.Fatalf("register json: %v", err)
	}
	if len(registered.Labels) != 1 || registered.Labels[0] != "self-hosted" {
		t.Fatalf("default label = %v", registered.Labels)
	}
	if rec := doAuth(h, http.MethodDelete, "/api/v1/admin/runners/"+registered.RunnerID, "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/runners/declare", `{}`, registered.Token); rec.Code != http.StatusUnauthorized {
		t.Fatalf("declare after revoke = %d", rec.Code)
	}
	// Declare with a malformed body and with another runner's token.
	_, token2 := registerRunner(t, e, h, admin)
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/runners/declare", `{"labels":"nope"}`, token2); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad declare body = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/runners/declare", `{}`, "wrt_YQ.abcdefgh1234.0123456789abcdef"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown runner token = %d", rec.Code)
	}
}

func TestSecretsEdges(t *testing.T) {
	e, h, cur := newHTTP(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	k, _ := NewKeyring(strings.Repeat("k", 32), "")
	e.svc.SetKeyring(k)
	// Name validation.
	for _, bad := range []string{"", "  ", "1TOKEN", "a-b", strings.Repeat("A", 65)} {
		if _, err := normalizeSecretName(bad); err == nil {
			t.Fatalf("normalizeSecretName(%q) must fail", bad)
		}
	}
	// Reader cannot write.
	*cur = auth.Principal{Name: "reader"}
	if rec := doAuth(h, http.MethodPut, "/o/r/api/threads/secrets/TOKEN", `{"value":"v"}`, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("reader put = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodDelete, "/o/r/api/threads/secrets/TOKEN", "", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("reader delete = %d", rec.Code)
	}
	*cur = admin
	// Validation + the write-only round trip through HTTP.
	if rec := doAuth(h, http.MethodPut, "/o/r/api/threads/secrets/bad-name", `{"value":"v"}`, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad name = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodPut, "/o/r/api/threads/secrets/TOKEN", `{"value":""}`, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty value = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodPut, "/o/r/api/threads/secrets/TOKEN", `{"other":"v"}`, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodPut, "/o/r/api/threads/secrets/TOKEN", `{"value":"v1"}`, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("put = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := doAuth(h, http.MethodDelete, "/o/r/api/threads/secrets/TOKEN", "", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	// A corrupt envelope fails the fetch (fail closed).
	if err := e.svc.SetSecret(ctx(), "o/r", "BROKEN", "value-1234", admin); err != nil {
		t.Fatalf("set: %v", err)
	}
	raw, _, _ := store.GetBytes(ctx(), e.store, SecretKey("o", "r", "BROKEN"), store.GetOptions{})
	var doc SecretDoc
	_ = json.Unmarshal(raw, &doc)
	doc.Sealed = "not-base64!!"
	if _, err := store.PutBytes(ctx(), e.store, SecretKey("o", "r", "BROKEN"), mustJSON(&doc), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.secretValues(ctx(), "o", "r"); err == nil {
		t.Fatal("a corrupt envelope must fail closed")
	}
	// openSecret rejects a short envelope and a wrong key.
	if _, err := e.svc.openSecret("o/r", "X", "AAAA"); err == nil {
		t.Fatal("short envelope must fail")
	}
	other, _ := NewKeyring(strings.Repeat("z", 32), "")
	good, _ := e.svc.sealSecret("o/r", "X", "value-1234")
	oldKey := e.svc.keys
	e.svc.SetKeyring(other)
	if _, err := e.svc.openSecret("o/r", "X", good); err == nil {
		t.Fatal("a different key must not open the envelope")
	}
	e.svc.SetKeyring(oldKey)
	// The masker skips short values and prefers the longest.
	if got := maskSecrets("abc", []string{"abc"}); got != "abc" {
		t.Fatalf("short values must not be masked: %q", got)
	}
	if got := maskSecrets("abcdefgh", []string{"abcd", "abcdefg"}); got != "***h" {
		t.Fatalf("longest-first masking = %q", got)
	}
	if got := maskSecrets("", []string{"abcd"}); got != "" {
		t.Fatal("empty input")
	}
	// A disabled instance answers 503 on the API.
	plain := newTestEnv(t)
	plainH := &Handler{Svc: plain.svc}
	plainH.Auth = func(*http.Request) (auth.Principal, *auth.AuthError) { return admin, nil }
	if rec := doAuth(plainH, http.MethodGet, "/o/r/api/threads/secrets", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("disabled list = %d", rec.Code)
	}
	if rec := doAuth(plainH, http.MethodPut, "/o/r/api/threads/secrets/TOKEN", `{"value":"v"}`, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled put = %d", rec.Code)
	}
}

func TestAttemptEdges(t *testing.T) {
	e, h, cur := newHTTP(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	*cur = admin
	run := e.mustCreate(t)[0]
	_, runnerToken := registerRunner(t, e, h, admin)
	job, _ := fetchJob(t, h, runnerToken)

	// start: bad ref, unknown repo/job, unknown job, wrong attempt. An instance
	// runner is not repository-scoped, so an unknown repository is a 404 (the
	// run record is absent), never the old repo-scope 403; the claim remains the
	// authority (a job another runner holds is still 403 below).
	goodRef := JobRef("o/r", run.ID, "j-build")
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/api/v1/threads/jobs/notaref/attempt/1/start", 404},
		{"/api/v1/threads/jobs/" + JobRef("x/y", run.ID, "j-build") + "/attempt/1/start", 404},
		{"/api/v1/threads/jobs/" + JobRef("o/r", run.ID, "j-nope") + "/attempt/1/start", 404},
		{"/api/v1/threads/jobs/" + goodRef + "/attempt/9/start", 409},
		{"/api/v1/threads/jobs/" + goodRef + "/attempt/abc/start", 400},
		{"/api/v1/threads/jobs/" + goodRef + "/attempt/1/bogus", 404},
	} {
		if rec := doAuth(h, http.MethodPost, tc.path, "", runnerToken); rec.Code != tc.want {
			t.Fatalf("%s = %d, want %d (%s)", tc.path, rec.Code, tc.want, rec.Body.String())
		}
	}
	rec := doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+goodRef+"/attempt/1/start", "", runnerToken)
	var start StartView
	if err := json.Unmarshal(rec.Body.Bytes(), &start); err != nil {
		t.Fatalf("start json: %v", err)
	}
	// A second runner cannot start a job claimed by the first.
	_, token2 := registerRunner(t, e, h, admin)
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+goodRef+"/attempt/1/start", "", token2); rec.Code != http.StatusForbidden {
		t.Fatalf("foreign start = %d", rec.Code)
	}
	// Log validation.
	logPath := "/api/v1/threads/jobs/" + goodRef + "/attempt/1/log"
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"seq":0,"text":"x"}`, 400},
		{`{"seq":1}`, 200}, // empty text is a legal (empty) frame
	} {
		if rec := doAuth(h, http.MethodPost, logPath, tc.body, start.Token); rec.Code != tc.want {
			t.Fatalf("log %s = %d, want %d", tc.body, rec.Code, tc.want)
		}
	}
	// complete with a bad conclusion and too many steps.
	completePath := "/api/v1/threads/jobs/" + goodRef + "/attempt/1/complete"
	if rec := doAuth(h, http.MethodPost, completePath, `{"conclusion":"maybe"}`, start.Token); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad conclusion = %d", rec.Code)
	}
	manySteps := make([]string, 513)
	for i := range manySteps {
		manySteps[i] = `{"name":"s","conclusion":"success"}`
	}
	if rec := doAuth(h, http.MethodPost, completePath, `{"conclusion":"success","steps":[`+strings.Join(manySteps, ",")+`]}`, start.Token); rec.Code != http.StatusBadRequest {
		t.Fatalf("too many steps = %d", rec.Code)
	}
	// A cancelled attempt is terminal: a later complete conflicts.
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+goodRef+"/attempt/1/cancel", "", start.Token); rec.Code != http.StatusOK {
		t.Fatalf("ack cancel = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodPost, completePath, `{"conclusion":"success"}`, start.Token); rec.Code != http.StatusConflict {
		t.Fatalf("complete after cancel = %d", rec.Code)
	}
	// Acknowledging a cancel twice is idempotent.
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/jobs/"+goodRef+"/attempt/1/cancel", "", start.Token); rec.Code != http.StatusOK {
		t.Fatalf("second ack = %d", rec.Code)
	}
	// Cancel/rerun on unknown runs and by a reader.
	*cur = auth.Principal{Name: "reader"}
	if rec := doAuth(h, http.MethodPost, "/o/r/api/threads/"+run.ID+"/rerun", "", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("reader rerun = %d", rec.Code)
	}
	*cur = admin
	if rec := doAuth(h, http.MethodPost, "/o/r/api/threads/1795176002000000000-abcdef/cancel", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown cancel = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodPost, "/o/r/api/threads/1795176002000000000-abcdef/rerun", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown rerun = %d", rec.Code)
	}
	// Log pages: unknown job, bad cursor pagination bounds.
	if rec := doAuth(h, http.MethodGet, "/o/r/api/threads/"+run.ID+"/jobs/j-nope/log?accept=json", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown job log = %d", rec.Code)
	}
	if rec := doAuth(h, http.MethodGet, "/o/r/api/threads/"+run.ID+"/jobs/j-build/log", "", ""); rec.Code != http.StatusOK {
		t.Fatalf("plain log request must answer JSON = %d", rec.Code)
	}
	if page, err := e.svc.LogFrames(ctx(), "o/r", run.ID, "j-build", 99, 0, 0, admin); err != nil || page.Attempt != 99 || len(page.Frames) != 0 {
		t.Fatalf("empty attempt page = %+v, %v", page, err)
	}
	e.roles.public = false
	if _, err := e.svc.LogFrames(ctx(), "o/r", run.ID, "j-build", 1, 0, 5, auth.Anonymous()); err == nil {
		t.Fatal("an anonymous log read on a private repo must fail")
	}
	e.roles.public = true
	if _, _, err := e.svc.loadJobAndRun(ctx(), "o", "r", "bad", "j-build"); err == nil {
		t.Fatal("a bad run id must fail")
	}
	if _, ok := logSeqOf("garbage", "prefix-"); ok {
		t.Fatal("logSeqOf must reject a bad key")
	}
	_ = job
}

func TestRunnerLivenessAndDeclare(t *testing.T) {
	e, h, cur := newHTTP(t)
	admin := auth.Principal{Name: "admin", Admin: true}
	*cur = admin
	id, token := registerRunner(t, e, h, admin)
	// Declare records version + labels and stamps last_seen.
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/runners/declare", `{"version":"1.2.3","labels":["GPU","linux"]}`, token); rec.Code != http.StatusOK {
		t.Fatalf("declare = %d", rec.Code)
	}
	runner, _, err := e.svc.loadRunner(ctx(), id)
	if err != nil {
		t.Fatalf("loadRunner: %v", err)
	}
	if runner.DeclaredVersion != "1.2.3" || len(runner.Labels) != 2 || runner.LastSeenAt == nil {
		t.Fatalf("runner = %+v", runner)
	}
	// The liveness refresh fires when last_seen is stale.
	e.advance(2 * time.Minute)
	if _, err := e.svc.authorizeRunner(ctx(), token); err != nil {
		t.Fatalf("authorize: %v", err)
	}
	refreshed, _, _ := e.svc.loadRunner(ctx(), id)
	if *refreshed.LastSeenAt <= *runner.LastSeenAt {
		t.Fatalf("last_seen not refreshed: %q → %q", *runner.LastSeenAt, *refreshed.LastSeenAt)
	}
	if !e.svc.olderThan("nonsense", time.Minute) {
		t.Fatal("a bad stamp is older")
	}
	// Corrupt runner records fail the bearer.
	if _, err := store.PutBytes(ctx(), e.store, RunnerKey(id), []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/runners/declare", `{}`, token); rec.Code != http.StatusInternalServerError {
		t.Fatalf("corrupt runner declare = %d", rec.Code)
	}
	// Corrupt credentials too.
	credKey := RunnerTokenKey(id)
	if _, err := store.PutBytes(ctx(), e.store, credKey, []byte("{"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if rec := doAuth(h, http.MethodPost, "/api/v1/threads/runners/declare", `{}`, token); rec.Code != http.StatusInternalServerError {
		t.Fatalf("corrupt credential declare = %d", rec.Code)
	}
}

func TestRunnerProtocolMethodGates(t *testing.T) {
	_, h, _ := newHTTP(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/threads/runners/register"},
		{http.MethodGet, "/api/v1/threads/runners/declare"},
		{http.MethodGet, "/api/v1/threads/runners/fetch"},
		{http.MethodGet, "/api/v1/threads/runners/bogus"},
		{http.MethodGet, "/api/v1/threads/jobs/jr_x/attempt/1/start"},
		{http.MethodGet, "/api/v1/threads/runners"},
	} {
		rec := doAuth(h, tc.method, tc.path, "", "")
		if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s = %d", tc.method, tc.path, rec.Code)
		}
	}
	// /api-browser is not a protocol lane.
	req := httptest.NewRequest(http.MethodPost, "/api-browser/v1/threads/runners/fetch", nil)
	if h.Handle(httptest.NewRecorder(), req) {
		t.Fatal("the protocol must not claim the api-browser lane")
	}
	// Repo admin method gates.
	for _, tc := range []struct{ method, path string }{
		{http.MethodDelete, "/o/r/api/threads/secrets"},
		{http.MethodPost, "/api/v1/admin/runners"},
		{http.MethodGet, "/api/v1/admin/runners/tokens"},
		{http.MethodGet, "/o/r/api/threads/" + "1795176002000000000-abcdef" + "/cancel"},
		{http.MethodPost, "/o/r/api/threads/" + "1795176002000000000-abcdef" + "/jobs/j-build/log"},
	} {
		if rec := doAuth(h, tc.method, tc.path, "", ""); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s = %d", tc.method, tc.path, rec.Code)
		}
	}
}

func TestGCRequeuesExpiredClaims(t *testing.T) {
	e := newTestEnv(t)
	run := e.mustCreate(t)[0]
	runner := &runnerAuth{Doc: &RunnerDoc{ID: "rnr", Labels: []string{"linux"}, AllowAll: true, State: RunnerActive}}
	job, err := e.svc.FetchJob(ctx(), runner, time.Millisecond)
	if err != nil || job == nil {
		t.Fatalf("fetch = %+v, %v", job, err)
	}
	e.advance(time.Hour)
	n, err := e.svc.GC(ctx(), "o", "r")
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if n < 1 {
		t.Fatalf("GC swept %d", n)
	}
	jobs, _ := e.svc.loadJobs(ctx(), "o", "r", run.ID)
	for _, j := range jobs {
		if j.State == JobRunning {
			t.Fatalf("a lapsed claim must be requeued by GC: %+v", j)
		}
	}
	// GC on a repo with no queue is a no-op.
	if n, err := e.svc.GC(ctx(), "o", "other"); n != 0 || err != nil {
		t.Fatalf("empty GC = %d, %v", n, err)
	}
}
