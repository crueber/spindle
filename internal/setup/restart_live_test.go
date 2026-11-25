// restart_live_test.go — the restart diff obeys the read-live key list: a
// changed key that takes effect without a restart (the import SSRF policy) is
// NOT named in the "Restart required" list, while every construction-snapshot
// key still is.
package setup

import (
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/config"
)

func TestRestartKeysOmitReadLiveKeys(t *testing.T) {
	base := config.Defaults()
	cand := config.Defaults() // a whole-file save of the same values
	cand.Import.AllowPrivateNetworks = true
	cand.Import.URLAllowlist = []string{"github.com"}
	cand.Import.AllowFileURLs = true

	if keys := RestartKeys(base, cand); len(keys) != 0 {
		t.Fatalf("RestartKeys = %v, want no restart keys for the read-live import policy", keys)
	}

	// A construction-snapshot key in the same save is still named, and only
	// that one: the live keys must not mask it.
	cand.Import.MaxConcurrent = 4
	cand.Server.Listen = "127.0.0.1:9999"
	got := strings.Join(RestartKeys(base, cand), ",")
	if !strings.Contains(got, "import.max_concurrent") || !strings.Contains(got, "server.listen") {
		t.Fatalf("RestartKeys = %q, want the restart-required keys", got)
	}
	for _, live := range []string{"import.allow_private_networks", "import.url_allowlist", "import.allow_file_urls"} {
		if strings.Contains(got, live) {
			t.Fatalf("RestartKeys = %q, must not name the read-live %s", got, live)
		}
	}

	// The other read-live keys behave the same way (the filter is the declared
	// list, not a special case for import).
	base2 := config.Defaults()
	cand2 := config.Defaults()
	cand2.Telemetry.LogFormat = "json"
	cand2.Maintenance.Interval = config.Duration(90_000_000_000)
	if keys := RestartKeys(base2, cand2); len(keys) != 0 {
		t.Fatalf("RestartKeys = %v, want the read-live telemetry/maintenance keys omitted", keys)
	}
}
