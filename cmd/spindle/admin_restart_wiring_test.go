package main

// admin_restart_wiring_test.go — composition binds AdminDeps.Restart in BOTH
// boot modes (docs/PLAN-ADMIN.md §5 as amended). The restricted boot is the
// recovery path: after the fixed file is saved, only a restart puts it in force.
// The binding is checked, never called — a test must not signal its process.

import (
	"testing"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/server"
	"github.com/crueber/spindle/internal/webui"
)

func TestAdminRestartSeamBoundBothBootModes(t *testing.T) {
	for _, tc := range []struct {
		name string
		boot server.BootState
	}{
		{"normal boot", server.BootState{Mode: "normal", FileState: "valid"}},
		{"restricted boot", server.BootState{Mode: "setup_only", FileState: "invalid"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(func() { webui.BindAdmin(webui.AdminDeps{}) })
			wireAdminPages(config.Defaults(), tc.boot, nil, nil, nil, nil, nil, nil, nil)
			d := webui.AdminBindings()
			if d.Restart == nil {
				t.Fatal("AdminDeps.Restart is nil: POST /admin/restart would 503 and a saved config could never take effect")
			}
			if d.Access == nil {
				t.Fatal("AdminDeps.Access is nil: the restart control would lose its gate")
			}
		})
	}
}
