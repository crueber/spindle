package setup

// anchor_test.go — AnchorOf is the page's one key→section coordinate system
// (PLAN-ADMIN §4.6): every schema key must map to the card it renders in, and
// the mapping is total over BuildSchema.

import (
	"testing"

	"github.com/crueber/spindle/internal/config"
)

func TestAnchorOfIsTotalOverSchema(t *testing.T) {
	groups := BuildSchema(config.Defaults(), config.Defaults())
	if len(groups) == 0 {
		t.Fatal("empty schema")
	}
	seen := 0
	for _, g := range groups {
		for _, k := range g.Keys {
			seen++
			if got, want := AnchorOf(k.Key), g.Section; got != want {
				t.Errorf("AnchorOf(%q) = %q, want %q", k.Key, got, want)
			}
		}
	}
	// Totality is the invariant, not a fixed count: the schema grows with
	// features (artifacts and the editor each added keys since the plan's
	// appendix was measured), and pinning 149 made a new config key fail a
	// test that is about coverage of the map. The tour above already proves
	// every key maps to its own card; this only guards against an empty walk.
	if seen < 100 {
		t.Fatalf("schema keys = %d, want a populated schema", seen)
	}
}

func TestAnchorOf(t *testing.T) {
	cases := map[string]string{
		"server.auth.mode":          "auth",
		"server.auth.tokens":        "auth",
		"server.listen":             "server",
		"cache.max_bytes":           "cache",
		"store.s3.force_path_style": "store",
		"threads.retention_days":    "threads",
		"server":                    "server",
		"":                          "",
	}
	for key, want := range cases {
		if got := AnchorOf(key); got != want {
			t.Errorf("AnchorOf(%q) = %q, want %q", key, got, want)
		}
	}
}
