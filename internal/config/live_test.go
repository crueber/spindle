// live_test.go — the live [import] SSRF policy holder (live.go): the admin
// save publishes the saved section, the environment keeps winning over the
// file, and an unwired holder leaves every caller on its startup copy.
package config

import (
	"strings"
	"sync"
	"testing"
)

func TestLiveImportGetSet(t *testing.T) {
	// An unwired holder is the nil-pointer case: no panic, ok=false, and
	// ImportPolicy falls back to the startup copy.
	var unwired *LiveImport
	if _, ok := unwired.Get(); ok {
		t.Fatal("nil holder must report ok=false")
	}
	startup := Defaults()
	startup.Import.AllowFileURLs = true
	if got := ImportPolicy(unwired, startup); !got.AllowFileURLs {
		t.Fatalf("unwired ImportPolicy = %+v, want the startup section", got)
	}
	if got := ImportPolicy(unwired, nil); got.AllowPrivateNetworks || got.AllowFileURLs || len(got.URLAllowlist) != 0 {
		t.Fatalf("unwired ImportPolicy(nil startup) = %+v, want the fail-closed defaults", got)
	}

	live := NewLiveImport(startup)
	got, ok := live.Get()
	if !ok || !got.AllowFileURLs {
		t.Fatalf("NewLiveImport = %+v ok=%v, want the snapshot", got, ok)
	}
	if got := ImportPolicy(live, nil); !got.AllowFileURLs {
		t.Fatalf("wired ImportPolicy = %+v, want the live section", got)
	}

	// NewLiveImport(nil) is the compiled defaults, never a zero section that
	// reads as "allowlist empty + not private"... which is the same shape, so
	// assert against Defaults explicitly.
	if got, _ := NewLiveImport(nil).Get(); got.MaxRefs != Defaults().Import.MaxRefs {
		t.Fatalf("NewLiveImport(nil) = %+v, want Defaults().Import", got)
	}

	// Set publishes; a caller that keeps mutating its own value cannot reach
	// the published copy (the URLAllowlist slice is copied too).
	next := Import{AllowPrivateNetworks: true, URLAllowlist: []string{"github.com"}}
	live.Set(next)
	next.URLAllowlist[0] = "mutated.example"
	if got, _ := live.Get(); !got.AllowPrivateNetworks || got.URLAllowlist[0] != "github.com" {
		t.Fatalf("Set did not copy: %+v", got)
	}
	// The nil receiver stays a no-op (an unwired save path).
	unwired.Set(Import{AllowPrivateNetworks: true})
}

func TestLiveImportIsRaceFree(t *testing.T) {
	live := NewLiveImport(Defaults())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				live.Set(Import{AllowPrivateNetworks: i%2 == 0, URLAllowlist: []string{"a.example", "b.example"}})
			}
		}(i)
		go func() {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				_, _ = live.Get()
				_ = ImportPolicy(live, nil)
			}
		}()
	}
	wg.Wait()
}

func TestPublishFileSaveEnvironmentWins(t *testing.T) {
	startup := Defaults() // the boot truth: private refused, no allowlist
	live := NewLiveImport(startup)
	candidate := Defaults()
	candidate.Import.AllowPrivateNetworks = true
	candidate.Import.URLAllowlist = []string{"github.com"}
	candidate.Import.AllowFileURLs = true

	// No overlay: the saved file is the policy.
	got, err := live.PublishFileSave(candidate, nil)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if !got.AllowPrivateNetworks || !got.AllowFileURLs || len(got.URLAllowlist) != 1 {
		t.Fatalf("published = %+v, want the file candidate", got)
	}
	if live, _ := live.Get(); !live.AllowPrivateNetworks {
		t.Fatalf("live = %+v, want the published candidate", live)
	}

	// The environment supplies the pre-save value: a file save that says
	// true must NOT override it (the boot ladder is defaults → file → env).
	environ := []string{
		"SPINDLE__IMPORT__ALLOW_PRIVATE_NETWORKS=false",
		"SPINDLE__IMPORT__ALLOW_FILE_URLS=true",
		`SPINDLE__IMPORT__URL_ALLOWLIST=["env.example"]`,
	}
	got, err = live.PublishFileSave(candidate, environ)
	if err != nil {
		t.Fatalf("publish with overlay: %v", err)
	}
	if got.AllowPrivateNetworks {
		t.Fatal("env-supplied false must win over the file's true")
	}
	if !got.AllowFileURLs || len(got.URLAllowlist) != 1 || got.URLAllowlist[0] != "env.example" {
		t.Fatalf("published = %+v, want the env overlay applied", got)
	}

	// Legacy WALGIT__ spelling works, and SPINDLE__ wins over it.
	got, err = live.PublishFileSave(candidate, []string{
		"WALGIT__IMPORT__ALLOW_PRIVATE_NETWORKS=true",
		"SPINDLE__IMPORT__ALLOW_PRIVATE_NETWORKS=false",
	})
	if err != nil || got.AllowPrivateNetworks {
		t.Fatalf("primary must win over legacy: %+v %v", got, err)
	}

	// Non-import keys are ignored on purpose: a struct-slice env key that
	// only exists on the file side must not make a save fail here.
	got, err = live.PublishFileSave(candidate, []string{
		"SPINDLE__IMPORT__ALLOW_PRIVATE_NETWORKS=true",
		"SPINDLE__SERVER__AUTH__TOKENS__0__NAME=ops",
		"SPINDLE__STORE__BUCKET=acme",
	})
	if err != nil {
		t.Fatalf("non-import keys must be ignored: %v", err)
	}
	if !got.AllowPrivateNetworks {
		t.Fatalf("published = %+v, want the import key applied", got)
	}
}

func TestPublishFileSaveFailuresChangeNothing(t *testing.T) {
	live := NewLiveImport(Defaults())

	// An unparseable import overlay is the only reachable failure: the file is
	// written, nothing is published, and the previous policy stays in force.
	candidate := Defaults()
	candidate.Import.AllowPrivateNetworks = true
	if _, err := live.PublishFileSave(candidate, []string{"SPINDLE__IMPORT__URL_ALLOWLIST=not-a-list"}); err == nil {
		t.Fatal("a bad overlay value must fail")
	}
	if after, _ := live.Get(); after.AllowPrivateNetworks {
		t.Fatalf("failed publish changed the policy: %+v", after)
	}
	// An unknown import key is an ignored override (boot rule), not a
	// failure: the save still publishes the file value.
	got, err := live.PublishFileSave(candidate, []string{"SPINDLE__IMPORT__BOGUS=true"})
	if err != nil || !got.AllowPrivateNetworks {
		t.Fatalf("unknown key: %+v %v", got, err)
	}

	// A nil candidate publishes nothing and reports the current policy.
	if got, err := live.PublishFileSave(nil, nil); err != nil || !got.AllowPrivateNetworks {
		t.Fatalf("nil candidate: %+v %v", got, err)
	}
	// A nil holder is a no-op (the unwired save path).
	var unwired *LiveImport
	if _, err := unwired.PublishFileSave(candidate, nil); err != nil {
		t.Fatalf("nil holder publish: %v", err)
	}
}

// TestPublishFileSaveErrorNamesTheEnvVar keeps the log line actionable.
func TestPublishFileSaveErrorNamesTheEnvVar(t *testing.T) {
	var holder LiveImport
	_, err := holder.PublishFileSave(Defaults(), []string{"SPINDLE__IMPORT__ALLOW_PRIVATE_NETWORKS=maybe"})
	if err == nil || !strings.Contains(err.Error(), "SPINDLE__IMPORT__ALLOW_PRIVATE_NETWORKS") {
		t.Fatalf("error = %v, want the env var named", err)
	}
	if !strings.Contains(err.Error(), "allow_private_networks") {
		t.Fatalf("error = %v, want the key named", err)
	}
}
