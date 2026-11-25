// live_policy_test.go — the push-mirror gate reads the live [import] policy
// when composition wires it, and its boot snapshot otherwise.
package pushmirror

import (
	"testing"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/repoimport"
)

func TestSSRFPolicyReadsLiveImport(t *testing.T) {
	live := config.NewLiveImport(nil)
	svc := New(Deps{LiveImport: live})

	fileSrc, err := repoimport.NormalizeSource("file:///srv/src.git")
	if err != nil {
		t.Fatal(err)
	}
	if cerr := (&Handler{Svc: svc}).checkSSRF(fileSrc, false); cerr == nil {
		t.Fatal("file:// must be refused before the publish")
	}
	live.Set(config.Import{AllowFileURLs: true})
	if cerr := (&Handler{Svc: svc}).checkSSRF(fileSrc, false); cerr != nil {
		t.Fatalf("file:// refused after the publish: %v", cerr)
	}
	if p := svc.ssrfPolicy(true); !p.AllowFile || !p.Dangerous {
		t.Fatalf("policy = %+v, want the live section with the per-call dangerous flag", p)
	}
}

func TestSSRFPolicySnapshotWhenUnwired(t *testing.T) {
	svc := New(Deps{AllowFile: true, AllowPrivate: true, Allowlist: []string{"a.example"}})
	p := svc.ssrfPolicy(false)
	if !p.AllowFile || !p.AllowPrivate || p.Dangerous || len(p.Allowlist) != 1 {
		t.Fatalf("unwired policy = %+v, want the Deps snapshot", p)
	}
}
