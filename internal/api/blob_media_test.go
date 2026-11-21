package api

import "testing"

func TestRawContentType(t *testing.T) {
	cases := map[string]string{
		"docs/demo.webp": "image/webp",
		"a.PNG":          "image/png",
		"a.jpeg":         "image/jpeg",
		"a.gif":          "image/gif",
		"a.avif":         "image/avif",
		"a.bmp":          "image/bmp",
		"a.ico":          "image/x-icon",
		"a.mp4":          "video/mp4",
		"a.webm":         "video/webm",
		"a.ogv":          "video/ogg",
		"a.mp3":          "audio/mpeg",
		"a.wav":          "audio/wav",
		"a.ogg":          "audio/ogg",
		"a.m4a":          "audio/mp4",
		// text and active content stay text/plain (SVG can carry script)
		"README.md": "text/plain; charset=utf-8",
		"a.svg":     "text/plain; charset=utf-8",
		"a.html":    "text/plain; charset=utf-8",
		"a":         "text/plain; charset=utf-8",
	}
	for p, want := range cases {
		if got := rawContentType(p); got != want {
			t.Errorf("rawContentType(%q) = %q, want %q", p, got, want)
		}
	}
}
