package threads

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/store"
)

// getCount reports how many times the store was asked for one key.
func (s *scriptStore) getCount(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gets[key]
}

// resetOIDCKeyCache drops the warm cache (tests that replace the key object
// behind the service's back).
func resetOIDCKeyCache(s *Service) { s.oidcCache = &oidcKeyCache{} }

// TestOIDCKeyCacheBudget pins the cost model: cold = one GET of the key object;
// a warm JWKS read and a warm exchange add zero; TTL expiry re-reads once.
func TestOIDCKeyCacheBudget(t *testing.T) {
	e, now := oidcEnv(t, oidcWorkflow)
	oidcRun(t, e)
	job := fetchOIDC(t, e, "rnr-oidc-cache")

	st := &scriptStore{Memory: e.store, gets: map[string]int{}}
	e.svc.Store = st
	key := OIDCSigningKeyPath()

	// Cold exchange: exactly one key GET (the store also serves the token
	// record, job and run, which are not the key family).
	exchangeOK(t, e, job, "aud")
	if got := st.getCount(key); got != 1 {
		t.Fatalf("cold exchange key GETs = %d, want 1", got)
	}
	// Warm exchange and warm JWKS: no key reads at all.
	exchangeOK(t, e, job, "aud")
	fetchJWKS(t, e)
	fetchJWKS(t, e)
	if got := st.getCount(key); got != 1 {
		t.Fatalf("warm paths key GETs = %d, want 1 (zero added)", got)
	}
	// TTL expiry re-reads once.
	*now = now.Add(oidcKeyCacheTTL + time.Second)
	fetchJWKS(t, e)
	if got := st.getCount(key); got != 2 {
		t.Fatalf("after TTL key GETs = %d, want 2", got)
	}
	// The reload is cached again.
	fetchJWKS(t, e)
	if got := st.getCount(key); got != 2 {
		t.Fatalf("second warm read key GETs = %d, want 2", got)
	}
}

// TestOIDCKeyCacheRotationInvalidatesLocally pins the rotation half: a local
// rotation stores the NEW set in the cache immediately (the next read adds no
// store traffic), and the cached JWKS keeps the retired key for the overlap.
func TestOIDCKeyCacheRotationInvalidatesLocally(t *testing.T) {
	e, now := oidcEnv(t, oidcWorkflow)
	_, pemStr := pemKey(t, oidcKeyBits)
	old := now.Add(-oidcRotationAge - time.Hour).Format(dateTimeFmt)
	if _, err := store.PutBytes(ctx(), e.store, OIDCSigningKeyPath(), mustJSON(&oidcKeyDoc{
		Version: 1, ActiveKID: "k-old", Keys: []oidcKey{
			{KID: "k-old", Alg: "RS256", PrivatePEM: pemStr, CreatedAt: old},
		},
	}), store.PutOptions{Mode: store.PutOverwrite}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	st := &scriptStore{Memory: e.store, gets: map[string]int{}}
	e.svc.Store = st
	key := OIDCSigningKeyPath()

	set, err := e.svc.oidcKeySet(ctx())
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if set.activeKID == "k-old" {
		t.Fatal("aged key kept signing")
	}
	cold := st.getCount(key)
	if cold < 1 {
		t.Fatalf("rotation did not read the key object (%d)", cold)
	}

	// The rotated set is in the cache: no further store traffic, and the
	// retired key is still in the JWKS for in-flight tokens.
	if _, err := e.svc.oidcKeySet(ctx()); err != nil {
		t.Fatalf("warm set: %v", err)
	}
	jwks, err := e.svc.oidcJWKS(ctx())
	if err != nil {
		t.Fatalf("warm jwks: %v", err)
	}
	if got := st.getCount(key); got != cold {
		t.Fatalf("post-rotation reads added %d key GETs", got-cold)
	}
	if len(jwks) != 2 {
		t.Fatalf("cached JWKS has %d keys, want old+new: %+v", len(jwks), jwks)
	}
	foundOld, foundNew := false, false
	for _, k := range jwks {
		if k.KID == "k-old" {
			foundOld = true
		}
		if k.KID == set.activeKID {
			foundNew = true
		}
	}
	if !foundOld || !foundNew {
		t.Fatalf("cached JWKS overlap wrong: %+v", jwks)
	}
}

// TestOIDCKeyCacheDoesNotCacheFaults: a store error is returned every time and
// never becomes a stale key.
func TestOIDCKeyCacheDoesNotCacheFaults(t *testing.T) {
	boom := errors.New("key read down")
	st := &scriptStore{Memory: store.NewMemory(), gets: map[string]int{}}
	st.getErr, st.err = OIDCSigningKeyPath(), boom
	svc := New(st, nil)
	for i := 0; i < 2; i++ {
		if _, err := svc.oidcKeySet(ctx()); !errors.Is(err, boom) {
			t.Fatalf("attempt %d = %v, want the store error", i+1, err)
		}
	}
	if got := st.getCount(OIDCSigningKeyPath()); got != 2 {
		t.Fatalf("fault path key GETs = %d, want 2 (errors are not cached)", got)
	}
}

// TestOIDCKeyCacheSingleFlight: a cold burst (the open JWKS endpoint is the
// exposure) reads the object once, not once per request.
func TestOIDCKeyCacheSingleFlight(t *testing.T) {
	st := &scriptStore{Memory: store.NewMemory(), gets: map[string]int{}}
	svc := New(st, nil)
	const n = 4
	kids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			set, err := svc.oidcKeySet(ctx())
			if err != nil {
				errs[i] = err
				return
			}
			kids[i] = set.activeKID
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
	}
	for i := 1; i < n; i++ {
		if kids[i] != kids[0] {
			t.Fatalf("cold burst disagrees on the key: %v", kids)
		}
	}
	if got := st.getCount(OIDCSigningKeyPath()); got != 1 {
		t.Fatalf("cold burst key GETs = %d, want 1", got)
	}
}

// TestOIDCKeyCacheLiteralService: a Service built as a literal (no New) gets a
// throwaway cache — still correct, just uncached. Production composition always
// uses New.
func TestOIDCKeyCacheLiteralService(t *testing.T) {
	svc := &Service{Store: store.NewMemory()}
	set1, err := svc.oidcKeySet(ctx())
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	set2, err := svc.oidcKeySet(ctx())
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if set1.activeKID == "" || set1.activeKID != set2.activeKID {
		t.Fatalf("literal service kid = %q / %q", set1.activeKID, set2.activeKID)
	}
}
