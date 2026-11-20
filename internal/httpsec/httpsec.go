// Package httpsec holds the response-header policy and the redirect-target
// rule shared by the HTTP lanes and the server-rendered pages (06 §8).
//
// It is a leaf package with stdlib imports only: the core server and the
// feature/page layers both depend on it, so it must never import either
// (AGENTS law 8).
package httpsec

import (
	"net/http"
	"net/url"
	"strings"
)

// ContentSecurityPolicy is the one policy every response carries. It matches
// what the pages actually load: htmx and the island bundle from 'self', the
// single external stylesheet, inline `style=` attributes (label swatches,
// progress widths — the only inline style form the templates use), images from
// this host plus http(s)/data markdown sources, and same-origin fetch/SSE.
// There are no inline scripts or inline event handlers, so script-src needs no
// nonce or hash.
const ContentSecurityPolicy = "default-src 'self'; script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; img-src 'self' https: http: data:; " +
	"frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'"

// Apply sets the shared header set on h. Existing values win, so a route with a
// stricter per-response policy (the release-asset byte lane) can set its own
// before the header is written.
func Apply(h http.Header) {
	setIfEmpty(h, "Content-Security-Policy", ContentSecurityPolicy)
	setIfEmpty(h, "X-Frame-Options", "DENY")
	setIfEmpty(h, "Referrer-Policy", "same-origin")
	setIfEmpty(h, "X-Content-Type-Options", "nosniff")
}

func setIfEmpty(h http.Header, name, value string) {
	if h.Get(name) == "" {
		h.Set(name, value)
	}
}

// SafeNext resolves a caller-supplied post-login/return target (the `next=`
// query field and the `next` form field). Only a rooted path on this host
// survives: a scheme ("https://host"), a scheme-relative form ("//host"), a
// backslash ("/\host", which browsers read as an authority), a control or
// whitespace character, and a percent-encoded backslash all fall back. An
// unusable fallback itself collapses to "/".
func SafeNext(next, fallback string) string {
	if fallback == "" || !localPath(fallback) {
		fallback = "/"
	}
	if next == "" || !localPath(next) {
		return fallback
	}
	return next
}

// localPath reports whether v is a rooted, same-host path that is safe to use
// as a Location value.
func localPath(v string) bool {
	if !strings.HasPrefix(v, "/") || strings.HasPrefix(v, "//") {
		return false
	}
	for _, r := range v {
		if r <= ' ' || r == 0x7f || r == '\\' {
			return false
		}
	}
	// The browser percent-decodes before the origin decision in some paths
	// (and proxies decode early): refuse decoded backslashes and an encoded
	// scheme-relative prefix too.
	decoded, err := url.PathUnescape(v)
	if err != nil {
		return false
	}
	if strings.Contains(decoded, "\\") || strings.HasPrefix(decoded, "//") {
		return false
	}
	u, err := url.Parse(v)
	if err != nil || u.IsAbs() || u.Host != "" || u.Scheme != "" {
		return false
	}
	return true
}
