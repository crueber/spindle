package web

import (
	"io/fs"
	"testing"
)

// TestDistNeverShipsHTMXSSEExtension pins the D-WEB-14 / foundations-audit
// hygiene rule: the htmx SSE extension is not our transport (web/src/islands/
// sse.js is) and must never be embedded or served. The build deletes a stale
// web/dist/htmx-ext-sse.js and both the Makefile and Dockerfile guard it; this
// test is the shipped-binary half — walk the embedded tree, so a regression
// fails `go test` even when the file is git-ignored.
func TestDistNeverShipsHTMXSSEExtension(t *testing.T) {
	err := fs.WalkDir(FilesFS(), "dist", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && d.Name() == "htmx-ext-sse.js" {
			t.Fatalf("web/dist/%s is embedded in the binary: the htmx SSE extension was removed (D-WEB-14); delete it and rebuild", d.Name())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded dist: %v", err)
	}
}
