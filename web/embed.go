// Package web embeds the built UI (16_packaging.md §1.2, D-WEB-14): web/dist is
// produced by `make web` — vite builds the stylesheet (app.css), esbuild the
// island bundle (ui.js) and the SDK (repos.js), and the vendored htmx files are
// copied in; concepts/ arrives with the stylesheet build. There is no SPA shell.
// .keep is committed so the embed resolves on fresh checkouts with no toolchain.
// Only the BUILT artifacts ship in the binary — sources stay out.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var Files embed.FS

// FilesFS exposes the embedded tree as fs.FS for tests that enumerate assets.
func FilesFS() fs.FS { return Files }
