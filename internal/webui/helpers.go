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
		// duration renders a wall time in seconds compactly ("45s", "1m 12s",
		// "1h 04m"); a zero/negative value renders empty so a never-started run
		// omits it.
		"duration": durationText,
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

// durationText renders a wall time in seconds compactly: "45s", "1m 12s",
// "1h 04m". Zero (or negative) renders empty so a never-started run omits the
// field instead of claiming "0s".
func durationText(seconds int64) string {
	if seconds <= 0 {
		return ""
	}
	d := time.Duration(seconds) * time.Second
	switch {
	case d < time.Minute:
		return itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return itoa(int(d.Minutes())) + "m " + pad2(int(d.Seconds())%60) + "s"
	default:
		return itoa(int(d.Hours())) + "h " + pad2(int(d.Minutes())%60) + "m"
	}
}

// pad2 zero-pads a small non-negative int ("4" → "04").
func pad2(n int) string {
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}

// relTime renders an RFC3339 stamp as a compact relative age ("3d ago"); a
// zero/blank stamp renders empty so callers can omit it. The server renders
// the value at request time — no island is needed (a minute of staleness on a
// listing is invisible; detail pages carry the absolute stamp in the title).
func relTime(stamp string) string {
	return relTimeAt(stamp, time.Now())
}

// relTimeAt is relTime against an explicit clock (the pulse card renders
// "computed 12m ago" from the request's clock, not the wall clock at render).
func relTimeAt(stamp string, now time.Time) string {
	if stamp == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return ""
	}
	d := now.Sub(t)
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
