package setup

// The Configuration-page round trip for the image policy keys
// (threads.image_allow_prefixes / threads.image_require_digest): the schema
// carries them, the form posts them, Coerce accepts the documented spellings,
// a save re-encodes them, and a changed value appears in RestartKeys
// (docs/PLAN-ARTIFACT-KEEPER.md §5.2).

import (
	"net/url"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/crueber/spindle/internal/config"
)

func TestThreadsImageSchemaRoundTrip(t *testing.T) {
	base := roundTripConfig(t)
	base.Threads.ImageAllowPrefixes = []string{"docker.io/library", "ak.example/dockerhub"}
	base.Threads.ImageRequireDigest = true
	groups := BuildSchema(base, config.Defaults())

	allow, err := findField(groups, "threads.image_allow_prefixes")
	if err != nil {
		t.Fatal(err)
	}
	if allow.Type != "list" {
		t.Fatalf("image_allow_prefixes type = %q, want list", allow.Type)
	}
	digest, err := findField(groups, "threads.image_require_digest")
	if err != nil {
		t.Fatal(err)
	}
	if digest.Type != "bool" {
		t.Fatalf("image_require_digest type = %q, want bool", digest.Type)
	}

	values := FormValues(groups)
	if got := values.Get("threads.image_allow_prefixes"); got != "docker.io/library, ak.example/dockerhub" {
		t.Fatalf("rendered allowlist = %q", got)
	}
	if got := values.Get("threads.image_require_digest"); got != "true" {
		t.Fatalf("rendered digest flag = %q", got)
	}
	// An untouched form is a no-op (the page's whole-schema save).
	cand, err := ParseValues(values, cloneConfig(t, base))
	if err != nil {
		t.Fatalf("ParseValues: %v", err)
	}
	if strings.Join(cand.Threads.ImageAllowPrefixes, ",") != "docker.io/library,ak.example/dockerhub" || !cand.Threads.ImageRequireDigest {
		t.Fatalf("round trip changed the values: %+v", cand.Threads)
	}
	if keys := RestartKeys(base, cand); len(keys) != 0 {
		t.Fatalf("an untouched save reported restart keys %v", keys)
	}

	// Edit both: each appears in RestartKeys (the policy is read at boot).
	edited := url.Values{}
	for k, v := range values {
		edited[k] = v
	}
	edited.Set("threads.image_allow_prefixes", "ghcr.io/acme")
	edited.Set("threads.image_require_digest", "false")
	cand, err = ParseValues(edited, cloneConfig(t, base))
	if err != nil {
		t.Fatalf("ParseValues(edited): %v", err)
	}
	keys := strings.Join(RestartKeys(base, cand), ",")
	for _, want := range []string{"threads.image_allow_prefixes", "threads.image_require_digest"} {
		if !strings.Contains(keys, want) {
			t.Fatalf("RestartKeys = %q, missing %s", keys, want)
		}
	}

	// An empty list is the compatibility case and encodes to nothing.
	edited.Set("threads.image_allow_prefixes", "")
	cand, err = ParseValues(edited, cloneConfig(t, base))
	if err != nil {
		t.Fatalf("ParseValues(empty): %v", err)
	}
	if len(cand.Threads.ImageAllowPrefixes) != 0 {
		t.Fatalf("empty list parsed as %v", cand.Threads.ImageAllowPrefixes)
	}
	var buf strings.Builder
	if err := toml.NewEncoder(&buf).Encode(cand); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(buf.String(), `image_allow_prefixes = [""]`) {
		t.Fatalf("empty list encoded a phantom entry:\n%s", buf.String())
	}
}

// TestThreadsImageValidationNamesTheKey: the page marks the offending row, so
// a bad entry must report threads.image_allow_prefixes — never a bare banner.
func TestThreadsImageValidationNamesTheKey(t *testing.T) {
	base := roundTripConfig(t)
	for _, bad := range []string{"https://ak.example/dockerhub", "ak.example/dockerhub/", "docker.io", "AK.example/dockerhub", "docker.io/library:latest"} {
		cand, err := ParseValues(url.Values{"threads.image_allow_prefixes": {bad}}, base)
		if err != nil {
			t.Fatalf("Coerce rejected %q before the validator could name it: %v", bad, err)
		}
		_, errs := config.Validate(cand)
		if len(errs) == 0 {
			t.Fatalf("%q passed the validator", bad)
		}
		named := false
		for _, err := range errs {
			if strings.Contains(err.Error(), "threads.image_allow_prefixes") {
				named = true
			}
		}
		if !named {
			t.Fatalf("%q: errors %v do not name the key", bad, errs)
		}
	}
}
