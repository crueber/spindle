package staticpages

import (
	"path"
	"strings"
)

// contentTypes is the pinned extension table (§4). It is deliberately small and
// closed: a file nobody anticipated is served as application/octet-stream, never
// sniffed. Keys are lowercase including the dot.
var contentTypes = map[string]string{
	".html":        "text/html",
	".htm":         "text/html",
	".css":         "text/css",
	".js":          "text/javascript",
	".mjs":         "text/javascript",
	".json":        "application/json",
	".map":         "application/json",
	".webmanifest": "application/manifest+json",
	".txt":         "text/plain",
	".md":          "text/markdown",
	".markdown":    "text/markdown",
	".csv":         "text/csv",
	".xml":         "text/xml",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".jpg":         "image/jpeg",
	".jpeg":        "image/jpeg",
	".gif":         "image/gif",
	".webp":        "image/webp",
	".avif":        "image/avif",
	".ico":         "image/x-icon",
	".pdf":         "application/pdf",
	".wasm":        "application/wasm",
	".woff2":       "font/woff2",
	".woff":        "font/woff",
	".ttf":         "font/ttf",
	".otf":         "font/otf",
	".eot":         "application/vnd.ms-fontobject",
	".mp3":         "audio/mpeg",
	".mp4":         "video/mp4",
	".webm":        "video/webm",
	".ogg":         "audio/ogg",
	".wav":         "audio/wav",
	".zip":         "application/zip",
	".gz":          "application/gzip",
	".ics":         "text/calendar",
}

// contentType maps a served path to its pinned media type. text/* and
// application/json carry an explicit UTF-8 charset (never sniffed); everything
// unknown is application/octet-stream.
func contentType(name string) string {
	ct, ok := contentTypes[strings.ToLower(path.Ext(name))]
	if !ok {
		return "application/octet-stream"
	}
	if strings.HasPrefix(ct, "text/") || ct == "application/json" {
		return ct + "; charset=utf-8"
	}
	return ct
}
