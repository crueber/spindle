// Package webui is the server-rendered page layer (docs/go/12_web_ui.md §2,
// D-WEB-14): Go html/template pages + htmx + small vanilla-ES islands.
//
// It is an edge package: it reads the same services the JSON API reads (the
// api.Env seams) and renders them, so a page and its API twin cannot drift on
// data. It never writes to the store by hand; pages call the feature services
// the composition injects.
package webui

import (
	"time"

	"github.com/crueber/walhub/internal/api"
	"github.com/crueber/walhub/internal/config"
)

// Env is the composition-provided environment. API carries the core seams
// (repo view, registry, visibility); feature services are added per migration
// group as their pages land.
type Env struct {
	Cfg     *config.Config
	API     *api.Env
	Version string

	// Now is the clock (tests override it). nil → time.Now.
	Now func() time.Time
}

func (e Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}
