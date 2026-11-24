// moderation_m4_wiring_test.go — composition binds M4's seam: the server's
// collab-write spam observer is the moderation service's scoring verb (nil
// without collab wiring), so the middleware never scores when the feature is
// not composed and never needs to import the feature package (law 8).
package main

import (
	"context"
	"testing"

	"github.com/crueber/spindle/internal/moderation"
	"github.com/crueber/spindle/internal/store"
)

func TestSpamObserverWiring(t *testing.T) {
	if got := spamObserverOf(nil); got != nil {
		t.Fatal("nil collab wiring bound an observer")
	}
	if got := spamObserverOf(&collabWiring{}); got != nil {
		t.Fatal("collab wiring without a moderation service bound an observer")
	}
	mod := moderation.New(store.NewMemory())
	c := &collabWiring{modSvc: mod}
	obs := spamObserverOf(c)
	if obs == nil {
		t.Fatal("collab wiring with a moderation service did not bind the observer")
	}
	// Below the threshold: observed, not filed.
	if obs(context.Background(), "acme", "widgets", "carol", "a body long enough to be scored, well over forty bytes") {
		t.Fatal("a single observation filed a report")
	}
}
