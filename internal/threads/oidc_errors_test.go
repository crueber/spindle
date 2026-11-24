package threads

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/store"
)

// putKeyDoc writes a key set verbatim (the error-path fixtures).
func putKeyDoc(t *testing.T, e *testEnv, doc *oidcKeyDoc) {
	t.Helper()
	if _, err := store.PutBytes(ctx(), e.svc.Store, OIDCSigningKeyPath(), mustJSON(doc),
		store.PutOptions{Mode: store.PutOverwrite, ContentType: "application/json"}); err != nil {
		t.Fatalf("put key doc: %v", err)
	}
}

// pemKey generates one RSA key and its PKCS#1 PEM.
func pemKey(t *testing.T, bits int) (*rsa.PrivateKey, string) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return priv, string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)}))
}

func TestOIDCKeySetCorruptObjects(t *testing.T) {
	e, _ := oidcEnv(t, oidcWorkflow)
	now := e.now

	// nil store
	if _, err := (&Service{}).oidcKeySet(ctx()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil store = %v", err)
	}

	// store read failure
	boom := errors.New("boom")
	if _, err := New(&errStore{Memory: e.store, getErr: boom}, nil).oidcKeySet(ctx()); !errors.Is(err, boom) {
		t.Fatalf("get error = %v", err)
	}

	// not JSON
	if err := e.svc.putCreate(ctx(), OIDCSigningKeyPath(), []byte("nope")); err != nil {
		t.Fatalf("seed corrupt: %v", err)
	}
	if _, err := e.svc.oidcKeySet(ctx()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt JSON = %v", err)
	}

	// unknown active kid
	if _, err := store.PutBytes(ctx(), e.svc.Store, OIDCSigningKeyPath(),
		[]byte(`{"version":1,"active_kid":"nope","keys":[{"kid":"k1","alg":"RS256","private_key_pem":"x","created_at":"2026-01-01T00:00:00Z"}]}`),
		store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.oidcKeySet(ctx()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("unknown kid = %v", err)
	}

	// empty key list / empty active kid
	for _, body := range []string{`{"version":1,"active_kid":"k","keys":[]}`, `{"version":1,"active_kid":"","keys":[]}`} {
		if _, err := store.PutBytes(ctx(), e.svc.Store, OIDCSigningKeyPath(), []byte(body),
			store.PutOptions{Mode: store.PutOverwrite}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := e.svc.oidcKeySet(ctx()); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("empty set %s = %v", body, err)
		}
	}

	_, pemStr := pemKey(t, oidcKeyBits)
	base := oidcKey{KID: "k1", Alg: "RS256", PrivatePEM: pemStr, CreatedAt: now.Format(dateTimeFmt)}

	// unparseable created_at
	badTime := base
	badTime.CreatedAt = "not-a-time"
	putKeyDoc(t, e, &oidcKeyDoc{Version: 1, ActiveKID: "k1", Keys: []oidcKey{badTime}})
	if _, err := e.svc.oidcKeySet(ctx()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("bad created_at = %v", err)
	}

	// not PEM
	notPEM := base
	notPEM.PrivatePEM = "not a pem"
	putKeyDoc(t, e, &oidcKeyDoc{Version: 1, ActiveKID: "k1", Keys: []oidcKey{notPEM}})
	if _, err := e.svc.oidcKeySet(ctx()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("bad PEM = %v", err)
	}

	// too-small RSA key
	small, smallPEM := pemKey(t, 1024)
	smallKey := base
	smallKey.PrivatePEM = smallPEM
	putKeyDoc(t, e, &oidcKeyDoc{Version: 1, ActiveKID: "k1", Keys: []oidcKey{smallKey}})
	if _, err := e.svc.oidcKeySet(ctx()); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("small key = %v", err)
	}
	if small.N.BitLen() != 1024 {
		t.Fatalf("fixture key = %d bits", small.N.BitLen())
	}
}

func TestOIDCKeySetWriteFailure(t *testing.T) {
	mem := store.NewMemory()
	boom := errors.New("no write")
	svc := New(&errStore{Memory: mem, putErr: boom}, nil)
	if _, err := svc.oidcKeySet(ctx()); !errors.Is(err, boom) {
		t.Fatalf("put error = %v", err)
	}
}

func TestOIDCRotationPrunesLongRetiredKeys(t *testing.T) {
	e, now := oidcEnv(t, oidcWorkflow)
	_, pemStr := pemKey(t, oidcKeyBits)
	old := now.Add(-oidcRotationAge - time.Hour).Format(dateTimeFmt)
	retired := now.Add(-2 * oidcRetiredRetention).Format(dateTimeFmt)
	putKeyDoc(t, e, &oidcKeyDoc{Version: 1, ActiveKID: "k-old", Keys: []oidcKey{
		{KID: "k-ancient", Alg: "RS256", PrivatePEM: pemStr, CreatedAt: old, RetiredAt: &retired},
		{KID: "k-old", Alg: "RS256", PrivatePEM: pemStr, CreatedAt: old},
	}})
	set, err := e.svc.oidcKeySet(ctx())
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if set.activeKID == "k-old" {
		t.Fatal("aged key kept signing")
	}
	if len(set.doc.Keys) != 2 {
		t.Fatalf("keys after rotation = %+v (the ancient retired key must be pruned)", set.doc.Keys)
	}
	// The freshly retired key is kept for the JWKS.
	if set.doc.Keys[0].RetiredAt == nil {
		t.Fatalf("old active key was not retired: %+v", set.doc.Keys[0])
	}
}

func TestOIDCMintRequestErrors(t *testing.T) {
	e, _ := oidcEnv(t, oidcWorkflow)
	if _, err := e.svc.mintIDTokenRequest(ctx(), "o", "r", "", "j-x", 1, time.Hour, "https://x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("attempt 0 = %v", err)
	}
	if _, err := e.svc.mintIDTokenRequest(ctx(), "o", "r", "run", "j-x", 1, time.Hour, "   "); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no base = %v", err)
	}
	// A non-positive TTL is capped to the documented maximum, not rejected.
	req, err := e.svc.mintIDTokenRequest(ctx(), "o", "r", "run", "j-x", 1, 0, "https://x/")
	if err != nil || req.RequestURL != "https://x/api/v1/threads/jobs/"+JobRef("o/r", "run", "j-x")+"/attempt/1/id-token" {
		t.Fatalf("mint = %+v, %v", req, err)
	}

	// Store failure.
	boom := errors.New("no write")
	bad := New(&errStore{Memory: e.store, putErr: boom}, nil)
	bad.Now = e.svc.Now
	if _, err := bad.mintIDTokenRequest(ctx(), "o", "r", "run", "j-x", 1, time.Hour, "https://x"); !errors.Is(err, boom) {
		t.Fatalf("put error = %v", err)
	}

	// Entropy failure.
	prev := randRead
	randRead = func([]byte) (int, error) { return 0, errors.New("no entropy") }
	defer func() { randRead = prev }()
	if _, err := e.svc.mintIDTokenRequest(ctx(), "o", "r", "run", "j-x", 1, time.Hour, "https://x"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("entropy = %v", err)
	}
	randRead = prev
	if _, err := newJTI(); err != nil {
		t.Fatalf("newJTI: %v", err)
	}
}

// seedRequestRecord writes a request-token record for (repo, run, job, attempt)
// with a known plaintext token.
func seedRequestRecord(t *testing.T, e *testEnv, owner, repo, run, job string, attempt int, token, expires string) {
	t.Helper()
	doc := idTokenRequestDoc{
		Version: 1, Repo: owner + "/" + repo, Run: run, Job: job, Attempt: attempt,
		TokenHash: hashSecret(token), CreatedAt: e.now.Format(dateTimeFmt), ExpiresAt: expires,
	}
	if _, err := store.PutBytes(ctx(), e.svc.Store, IDTokenRequestKey(owner, repo, run, job, attempt),
		mustJSON(&doc), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed record: %v", err)
	}
}

func TestOIDCExchangeDenialPaths(t *testing.T) {
	e, _ := oidcEnv(t, oidcWorkflow)
	run := oidcRun(t, e)
	job := fetchOIDC(t, e, "rnr-oidc-err")
	const tok = "wot_0123456789abcdef"

	// Long audience.
	if _, err := e.svc.ExchangeIDToken(ctx(), job.JobRef, job.Attempt, job.IDToken.RequestToken,
		strings.Repeat("a", 513), "https://spindle.example"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("long audience = %v", err)
	}

	// A record whose expiry is unreadable is corrupt, not a 401.
	seedRequestRecord(t, e, "o", "r", run.ID, job.JobID, job.Attempt, tok, "not-a-time")
	if _, err := e.svc.ExchangeIDToken(ctx(), job.JobRef, job.Attempt, tok, "aud", "https://spindle.example"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("bad expiry = %v", err)
	}

	// A record stored under this attempt that names another run is refused.
	otherRun := "1234567890123456789-abc123"
	mismatched := idTokenRequestDoc{
		Version: 1, Repo: "o/r", Run: otherRun, Job: job.JobID, Attempt: job.Attempt,
		TokenHash: hashSecret(tok), CreatedAt: e.now.Format(dateTimeFmt), ExpiresAt: e.now.Add(time.Hour).Format(dateTimeFmt),
	}
	if _, err := store.PutBytes(ctx(), e.svc.Store, IDTokenRequestKey("o", "r", run.ID, job.JobID, job.Attempt),
		mustJSON(&mismatched), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed mismatch: %v", err)
	}
	if _, err := e.svc.ExchangeIDToken(ctx(), job.JobRef, job.Attempt, tok, "aud", "https://spindle.example"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cross-run record = %v", err)
	}

	// Unknown job (no job object under the ref).
	ghost := JobRef("o/r", run.ID, "j-ghost")
	seedRequestRecord(t, e, "o", "r", run.ID, "j-ghost", 1, tok, e.now.Add(time.Hour).Format(dateTimeFmt))
	if _, err := e.svc.ExchangeIDToken(ctx(), ghost, 1, tok, "aud", "https://spindle.example"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown job = %v", err)
	}

	// Attempt mismatch: the job moved on after the token was minted.
	seedRequestRecord(t, e, "o", "r", run.ID, job.JobID, job.Attempt, tok, e.now.Add(time.Hour).Format(dateTimeFmt))
	if _, err := e.svc.UpdateJob(ctx(), "o/r", run.ID, job.JobID, JobUpdate{Attempt: job.Attempt + 1}); err != nil {
		t.Fatalf("bump attempt: %v", err)
	}
	if _, err := e.svc.ExchangeIDToken(ctx(), job.JobRef, job.Attempt, tok, "aud", "https://spindle.example"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("attempt mismatch = %v", err)
	}
}

func TestOIDCExchangeRunEdgeCases(t *testing.T) {
	// Missing run object: the job is there, its header is not.
	e, _ := oidcEnv(t, oidcWorkflow)
	run := oidcRun(t, e)
	job := fetchOIDC(t, e, "rnr-oidc-run1")
	if err := e.store.Delete(ctx(), RunKey("o", "r", run.ID), ""); err != nil {
		t.Fatalf("delete run: %v", err)
	}
	if _, err := e.svc.ExchangeIDToken(ctx(), job.JobRef, job.Attempt, job.IDToken.RequestToken, "aud", "https://spindle.example"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing run = %v", err)
	}

	// A run with neither ref nor branch cannot claim a subject.
	e2, _ := oidcEnv(t, oidcWorkflow)
	run2 := oidcRun(t, e2)
	job2 := fetchOIDC(t, e2, "rnr-oidc-run2")
	if _, err := e2.svc.casUpdate(ctx(), RunKey("o", "r", run2.ID), 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var r Run
		if err := json.Unmarshal(cur, &r); err != nil {
			return nil, false, err
		}
		r.Ref, r.Branch = "", ""
		return mustJSON(&r), true, nil
	}); err != nil {
		t.Fatalf("clear ref: %v", err)
	}
	if _, err := e2.svc.ExchangeIDToken(ctx(), job2.JobRef, job2.Attempt, job2.IDToken.RequestToken, "aud", "https://spindle.example"); !errors.Is(err, ErrUnprocessable) {
		t.Fatalf("no ref = %v", err)
	}
}

func TestOIDCExchangeCorruptKey(t *testing.T) {
	e, _ := oidcEnv(t, oidcWorkflow)
	oidcRun(t, e)
	job := fetchOIDC(t, e, "rnr-oidc-key")
	if err := e.svc.putCreate(ctx(), OIDCSigningKeyPath(), []byte("not json")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.ExchangeIDToken(ctx(), job.JobRef, job.Attempt, job.IDToken.RequestToken, "aud", "https://spindle.example"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt key = %v", err)
	}
}

func TestOIDCHandlerNoIssuer(t *testing.T) {
	e, _ := oidcEnv(t, oidcWorkflow)
	e.svc.CheckoutBaseURL = ""
	h := oidcHandler(e)
	for _, path := range []string{"/.well-known/openid-configuration", "/.well-known/jwks.json"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = ""
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if path == "/.well-known/jwks.json" {
			// The JWKS does not need an issuer (key material only).
			if rec.Code != http.StatusOK {
				t.Fatalf("%s = %d: %s", path, rec.Code, rec.Body.String())
			}
			continue
		}
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s = %d: %s", path, rec.Code, rec.Body.String())
		}
	}
	// Direct helper branch: a bare /.well-known is not ours.
	rec := httptest.NewRecorder()
	if h.handleWellKnown(rec, httptest.NewRequest(http.MethodGet, "/.well-known", nil), []string{".well-known"}) {
		t.Fatal("bare /.well-known claimed")
	}
}

func TestOIDCJWKSCorruptKey(t *testing.T) {
	e, _ := oidcEnv(t, oidcWorkflow)
	if err := e.svc.putCreate(ctx(), OIDCSigningKeyPath(), []byte("{bad")); err != nil {
		t.Fatalf("seed: %v", err)
	}
	rec := httptest.NewRecorder()
	oidcHandler(e).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("corrupt jwks = %d: %s", rec.Code, rec.Body.String())
	}

	// A key set that parses but whose PEM does not: the JWKS builder fails.
	badPEM := oidcKey{KID: "k1", Alg: "RS256", PrivatePEM: "nope", CreatedAt: e.now.Format(dateTimeFmt)}
	putKeyDoc(t, e, &oidcKeyDoc{Version: 1, ActiveKID: "k1", Keys: []oidcKey{badPEM}})
	rec = httptest.NewRecorder()
	oidcHandler(e).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("bad pem jwks = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOIDCExchangeNoIssuerHandler(t *testing.T) {
	e, _ := oidcEnv(t, oidcWorkflow)
	e.svc.CheckoutBaseURL = ""
	req := httptest.NewRequest(http.MethodPost, "/api/v1/threads/jobs/jr_x/attempt/1/id-token", strings.NewReader(`{"audience":"a"}`))
	req.Host = ""
	req.Header.Set("Authorization", "Bearer wot_x")
	rec := httptest.NewRecorder()
	oidcHandler(e).ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no-issuer exchange = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestOIDCExchangeRecordJSONCorrupt(t *testing.T) {
	e, _ := oidcEnv(t, oidcWorkflow)
	run := oidcRun(t, e)
	job := fetchOIDC(t, e, "rnr-oidc-json")
	if _, err := store.PutBytes(ctx(), e.svc.Store, IDTokenRequestKey("o", "r", run.ID, job.JobID, job.Attempt),
		[]byte("{not json"), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := e.svc.ExchangeIDToken(ctx(), job.JobRef, job.Attempt, job.IDToken.RequestToken, "aud", "https://spindle.example"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt record = %v", err)
	}
}

func TestOIDCStoreNilExchange(t *testing.T) {
	if _, err := (&Service{}).ExchangeIDToken(context.Background(), "jr_x", 1, "wot_x", "a", "https://x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("nil store exchange = %v", err)
	}
}

// scriptStore scripts per-key Get/Put behaviour for the OIDC error paths that
// only a racing instance (or a dying store) can produce.
type scriptStore struct {
	*store.Memory
	mu       sync.Mutex
	gets     map[string]int
	onGet    func(s *scriptStore, key string, n int)
	put412   string // key whose Put returns a precondition failure
	getErr   string // key whose Get returns an error
	getErrAt int    // Get number (1-based) that starts failing; 0 = always
	err      error
}

func newScriptStore() *scriptStore {
	return &scriptStore{Memory: store.NewMemory(), gets: map[string]int{}}
}

func (s *scriptStore) Get(ctx context.Context, key string, opts store.GetOptions) (store.GetResult, error) {
	s.mu.Lock()
	s.gets[key]++
	n := s.gets[key]
	hook := s.onGet
	s.mu.Unlock()
	if hook != nil {
		hook(s, key, n)
	}
	if s.getErr != "" && key == s.getErr && (s.getErrAt == 0 || n >= s.getErrAt) {
		return nil, s.err
	}
	return s.Memory.Get(ctx, key, opts)
}

func (s *scriptStore) Put(ctx context.Context, key string, body store.PutBody, opts store.PutOptions) (store.ObjectMeta, error) {
	if s.put412 != "" && key == s.put412 {
		return store.ObjectMeta{}, store.NewPrecondition(key, "")
	}
	return s.Memory.Put(ctx, key, body, opts)
}

func TestOIDCKeyCreateRaceLosesCleanly(t *testing.T) {
	// 412 and the winner's object is gone by the re-read: fail closed, never a
	// second trust root.
	boom := errors.New("read after 412")
	for _, tc := range []struct {
		name    string
		getErr  string
		wantErr error
	}{
		{"disappeared", "", ErrConflict},
		{"reread fails", OIDCSigningKeyPath(), nil}, // the injected error is compared below
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newScriptStore()
			st.put412 = OIDCSigningKeyPath()
			st.getErr, st.getErrAt, st.err = tc.getErr, 2, boom
			svc := New(st, nil)
			_, err := svc.oidcKeySet(ctx())
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			case !errors.Is(err, boom):
				t.Fatalf("err = %v, want the injected store error", err)
			}
		})
	}
}

func TestOIDCRotationRacingInstance(t *testing.T) {
	_, pemStr := pemKey(t, oidcKeyBits)
	old := time.Now().UTC().Add(-oidcRotationAge - time.Hour).Format(dateTimeFmt)
	fresh := time.Now().UTC().Format(dateTimeFmt)
	oldDoc := func(kid string) *oidcKeyDoc {
		return &oidcKeyDoc{Version: 1, ActiveKID: kid, Keys: []oidcKey{
			{KID: kid, Alg: "RS256", PrivatePEM: pemStr, CreatedAt: old},
		}}
	}
	cases := []struct {
		name string
		// replace writes the second read's object (nil = delete it).
		replace func(s *scriptStore)
		wantErr error
	}{
		{"another instance rotated", func(s *scriptStore) {
			other := oldDoc("k-fresh")
			other.Keys[0].CreatedAt = fresh
			_, _ = store.PutBytes(context.Background(), s.Memory, OIDCSigningKeyPath(), mustJSON(other), store.PutOptions{Mode: store.PutOverwrite})
		}, nil},
		{"corrupt second read", func(s *scriptStore) {
			_, _ = store.PutBytes(context.Background(), s.Memory, OIDCSigningKeyPath(), []byte("{bad"), store.PutOptions{Mode: store.PutOverwrite})
		}, ErrCorrupt},
		{"vanished", func(s *scriptStore) {
			_ = s.Memory.Delete(context.Background(), OIDCSigningKeyPath(), "")
		}, ErrCorrupt},
		{"unknown active kid", func(s *scriptStore) {
			bad := oldDoc("k-old")
			bad.ActiveKID = "nope"
			_, _ = store.PutBytes(context.Background(), s.Memory, OIDCSigningKeyPath(), mustJSON(bad), store.PutOptions{Mode: store.PutOverwrite})
		}, ErrCorrupt},
		{"bad created_at", func(s *scriptStore) {
			bad := oldDoc("k-old")
			bad.Keys[0].CreatedAt = "bad"
			_, _ = store.PutBytes(context.Background(), s.Memory, OIDCSigningKeyPath(), mustJSON(bad), store.PutOptions{Mode: store.PutOverwrite})
		}, ErrCorrupt},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st := newScriptStore()
			_, _ = store.PutBytes(ctx(), st.Memory, OIDCSigningKeyPath(), mustJSON(oldDoc("k-old")), store.PutOptions{Mode: store.PutOverwrite})
			st.onGet = func(s *scriptStore, key string, n int) {
				if key == OIDCSigningKeyPath() && n == 2 {
					tc.replace(s)
				}
			}
			svc := New(st, nil)
			set, err := svc.oidcKeySet(ctx())
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("racing rotation: %v", err)
			}
			if set.activeKID != "k-fresh" {
				t.Fatalf("active kid = %q, want the other instance's k-fresh", set.activeKID)
			}
		})
	}
}

func TestOIDCExchangeJobReadFailure(t *testing.T) {
	e, _ := oidcEnv(t, oidcWorkflow)
	run := oidcRun(t, e)
	job := fetchOIDC(t, e, "rnr-oidc-jobread")

	// Fail the job read (the token record read still succeeds).
	st := newScriptStore()
	_, _ = store.PutBytes(ctx(), st.Memory, IDTokenRequestKey("o", "r", run.ID, job.JobID, job.Attempt),
		mustJSON(&idTokenRequestDoc{
			Version: 1, Repo: "o/r", Run: run.ID, Job: job.JobID, Attempt: job.Attempt,
			TokenHash: hashSecret(job.IDToken.RequestToken),
			CreatedAt: e.now.Format(dateTimeFmt), ExpiresAt: e.now.Add(time.Hour).Format(dateTimeFmt),
		}), store.PutOptions{Mode: store.PutOverwrite})
	boom := errors.New("job read down")
	st.getErr, st.err = JobKey("o", "r", run.ID, job.JobID), boom
	e.svc.Store = st
	if _, err := e.svc.ExchangeIDToken(ctx(), job.JobRef, job.Attempt, job.IDToken.RequestToken, "aud", "https://spindle.example"); !errors.Is(err, boom) {
		t.Fatalf("job read error = %v", err)
	}
}

func TestOIDCAttemptAndRefEdges(t *testing.T) {
	e, _ := oidcEnv(t, oidcWorkflow)
	run := oidcRun(t, e)
	job := fetchOIDC(t, e, "rnr-oidc-edge")
	if _, err := e.svc.ExchangeIDToken(ctx(), job.JobRef, 0, job.IDToken.RequestToken, "aud", "https://spindle.example"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("attempt 0 = %v", err)
	}

	// Ref empty, branch present: the subject is built from the branch.
	e2, _ := oidcEnv(t, oidcWorkflow)
	run2 := oidcRun(t, e2)
	job2 := fetchOIDC(t, e2, "rnr-oidc-edge2")
	if _, err := e2.svc.casUpdate(ctx(), RunKey("o", "r", run2.ID), 3, func(cur []byte, _ store.Version) ([]byte, bool, error) {
		var r Run
		if err := json.Unmarshal(cur, &r); err != nil {
			return nil, false, err
		}
		r.Ref = ""
		return mustJSON(&r), true, nil
	}); err != nil {
		t.Fatalf("clear ref: %v", err)
	}
	token, _, claims := exchangeOK(t, e2, job2, "aud")
	if claims["ref"] != "refs/heads/main" || claims["sub"] != "repo:o/r:ref:refs/heads/main" {
		t.Fatalf("branch-derived ref = %v / %v", claims["ref"], claims["sub"])
	}
	_, _, sig := splitJWT(t, token)
	if len(sig) == 0 {
		t.Fatal("token has no signature")
	}
	_ = run
}

func TestOIDCJWKSRetiredBadKeyFails(t *testing.T) {
	e, _ := oidcEnv(t, oidcWorkflow)
	_, pemStr := pemKey(t, oidcKeyBits)
	retired := e.now.Format(dateTimeFmt)
	putKeyDoc(t, e, &oidcKeyDoc{Version: 1, ActiveKID: "k1", Keys: []oidcKey{
		{KID: "k1", Alg: "RS256", PrivatePEM: pemStr, CreatedAt: e.now.Format(dateTimeFmt)},
		{KID: "k-bad", Alg: "RS256", PrivatePEM: "nope", CreatedAt: e.now.Format(dateTimeFmt), RetiredAt: &retired},
	}})
	rec := httptest.NewRecorder()
	oidcHandler(e).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/jwks.json", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("retired bad key jwks = %d: %s", rec.Code, rec.Body.String())
	}

	// A PEM block of the wrong type / undecodable DER.
	wrongType := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("garbage")}))
	badDER := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: []byte("garbage")}))
	for _, body := range []string{wrongType, badDER} {
		resetOIDCKeyCache(e.svc) // each seed replaces the object under the warm cache
		k := oidcKey{KID: "k1", Alg: "RS256", PrivatePEM: body, CreatedAt: e.now.Format(dateTimeFmt)}
		putKeyDoc(t, e, &oidcKeyDoc{Version: 1, ActiveKID: "k1", Keys: []oidcKey{k}})
		if _, err := e.svc.oidcKeySet(ctx()); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("bad key body = %v", err)
		}
	}
}
