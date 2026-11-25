// live_policy_test.go — the mirror gate reads the live [import] policy when
// composition wires it (the admin save publishes there), and keeps the boot
// snapshot when it does not (tests, a one-shot process).
package mirror

import (
	"testing"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/repoimport"
)

func TestSSRFPolicyReadsLiveImport(t *testing.T) {
	live := config.NewLiveImport(nil) // the compiled defaults: deny everything
	svc := New(Deps{LiveImport: live, AllowFile: false})

	fileSrc, err := repoimport.NormalizeSource("file:///srv/src.git")
	if err != nil {
		t.Fatal(err)
	}
	if p := svc.ssrfPolicy(false); p.AllowFile || p.AllowPrivate || len(p.Allowlist) != 0 {
		t.Fatalf("unpublished policy = %+v, want the fail-closed defaults", p)
	}
	if cerr := (&Handler{Svc: svc}).checkSSRF(fileSrc, false); cerr == nil {
		t.Fatal("file:// must be refused before the publish")
	}

	// The admin save's publish (same section shape) is what the next request reads.
	live.Set(config.Import{
		AllowPrivateNetworks: true,
		AllowFileURLs:        true,
		URLAllowlist:         []string{"github.com"},
	})
	p := svc.ssrfPolicy(true)
	if !p.AllowFile || !p.AllowPrivate || !p.Dangerous || len(p.Allowlist) != 1 || p.Allowlist[0] != "github.com" {
		t.Fatalf("published policy = %+v, want the live section", p)
	}
	if cerr := (&Handler{Svc: svc}).checkSSRF(fileSrc, false); cerr != nil {
		t.Fatalf("file:// refused after the publish: %v", cerr)
	}
}

func TestSSRFPolicySnapshotWhenUnwired(t *testing.T) {
	svc := New(Deps{AllowFile: true, AllowPrivate: true, Allowlist: []string{"a.example"}})
	p := svc.ssrfPolicy(false)
	if !p.AllowFile || !p.AllowPrivate || p.Dangerous || len(p.Allowlist) != 1 {
		t.Fatalf("unwired policy = %+v, want the Deps snapshot", p)
	}
	if got := svc.ssrfPolicy(true); !got.Dangerous {
		t.Fatal("the per-call dangerous flag must survive the unwired path")
	}
}
