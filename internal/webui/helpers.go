package webui

import (
	"html/template"
	"net/http"
	"strings"
	"time"
)

// funcs is the template function map. Keep it small: anything that needs
// business logic belongs in the handler, not in a template.
func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		"rel":      relTime,
		"shortsha": shortSHA,
		"short":    shortSHA,
		// runDot maps a Threads run/job state onto the shared check-dot
		// vocabulary (15 §6; a pure display projection).
		"runDot": runDotState,
		// NOTE: html/template's builtin eq/ne/lt… are deliberately NOT shadowed:
		// the error page compares an int status (eq .Data.Status 401) and a
		// string-only shadow made every non-200 page fail to render.
		"hasPrefix": func(s, p string) bool { return strings.HasPrefix(s, p) },
		"plural": func(n int, one, many string) string {
			if n == 1 {
				return one
			}
			return many
		},
		// Server-side renderers (ported from the SPA's client libraries):
		// markdown/highlight/diff/graph all escape user text and return markup.
		"md": func(src string) template.HTML { return RenderMarkdown(src) },
		"mdrepo": func(src, owner, repo string) template.HTML {
			return RenderMarkdownIn(src, owner, repo)
		},
		// mdat renders markdown at a repo coordinate so relative links and
		// images resolve against the document's ref/dir (release notes, PR
		// descriptions). An empty ref degrades to plain mdrepo behaviour.
		"mdat": func(src, owner, repo, ref, dir string) template.HTML {
			return RenderMarkdownCtx(src, MarkdownCtx{Owner: owner, Repo: repo, Ref: ref, Dir: dir})
		},
		"diff":      ParseUnifiedDiff,
		"graph":     RenderCommitGraph,
		"graphrows": RenderCommitGraphRows,
		"hl":        Highlight,
		"dict": func(kv ...any) map[string]any {
			m := make(map[string]any, len(kv)/2)
			for i := 0; i+1 < len(kv); i += 2 {
				k, _ := kv[i].(string)
				m[k] = kv[i+1]
			}
			return m
		},
	}
}

// relTime renders an RFC3339 stamp as a compact relative age ("3d ago"); a
// zero/blank stamp renders empty so callers can omit it. The server renders
// the value at request time — no island is needed (a minute of staleness on a
// listing is invisible; detail pages carry the absolute stamp in the title).
func relTime(stamp string) string {
	if stamp == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return itoa(int(d.Minutes())) + "m ago"
	case d < 24*time.Hour:
		return itoa(int(d.Hours())) + "h ago"
	case d < 30*24*time.Hour:
		return itoa(int(d.Hours()/24)) + "d ago"
	case d < 365*24*time.Hour:
		return itoa(int(d.Hours()/(24*30))) + "mo ago"
	default:
		return itoa(int(d.Hours()/(24*365))) + "y ago"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// shortSHA truncates a full hash for display (never for identity).
func shortSHA(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

// headerWantsText reports whether the caller asked for a plain-text body. The
// API's error convention is text/plain; browsers send text/html.
func headerWantsText(r *http.Request) bool {
	a := r.Header.Get("Accept")
	if a == "" {
		return false
	}
	return strings.Contains(a, "text/plain") && !strings.Contains(a, "text/html")
}
