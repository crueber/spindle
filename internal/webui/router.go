package webui

import (
	"net/http"
	"sort"
)

// pageGroups is the migration registry: each group file registers its mount
// function from an init(), so groups never edit this file (disjoint ownership).
var pageGroups []func(*Server)

func registerGroup(f func(*Server)) { pageGroups = append(pageGroups, f) }

// mount builds the page router. The sentinel pattern matches everything and
// writes nothing; Serve treats that as "not a page" and the caller answers the
// next lane.
func (s *Server) mount() {
	s.sentinel = func(http.ResponseWriter, *http.Request) {}
	s.mux.HandleFunc("/", s.sentinel)
	s.mux.HandleFunc("GET /explore", s.explore)
	for _, f := range pageGroups {
		f(s)
	}
}

// --- reference page: /explore (foundation; the public group owns the rest) ---

type ownerCard struct {
	Name  string
	Repos int
}

type exploreData struct {
	Owners     []ownerCard
	TotalRepos int
}

// explore answers GET /explore: the visible owners and their live repo counts.
// It reuses the API's visibility rule (VisibleOwners), so the page and the JSON
// twin can never disagree about what exists.
func (s *Server) explore(w http.ResponseWriter, r *http.Request) {
	p, ok := s.gated(w, r)
	if !ok {
		return
	}
	// The owner list is the page's data: an outage is a 503 in BOTH
	// representations (the text twin must not silently answer "no owners").
	var names []string
	if s.env.API != nil && s.env.API.Repos != nil {
		n, err := s.env.API.VisibleOwners(r.Context(), p)
		if err != nil {
			s.errorPage(w, r, http.StatusServiceUnavailable, "owners unavailable")
			return
		}
		names = n
	}
	// The text twin is addressable both ways: ?format=text and an explicit
	// Accept: text/plain (06 §3.3 promised the Accept branch; the page owns it).
	if r.URL.Query().Get("format") == "text" || headerWantsText(r) {
		s.exploreText(w, names)
		return
	}
	var rows []ownerCard
	total := 0
	counts := map[string]int{}
	if s.env.API != nil && s.env.API.Repos != nil {
		if c, cerr := s.env.API.Repos.OwnerRepoCounts(r.Context()); cerr == nil {
			counts = c
		}
	}
	for _, n := range names {
		rows = append(rows, ownerCard{Name: n, Repos: counts[n]})
		total += counts[n]
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	s.page(w, r, p, http.StatusOK, "public/explore", "Explore", exploreData{Owners: rows, TotalRepos: total})
}

// exploreText is the ?format=text twin (07 §8): plain text for curl/agents.
func (s *Server) exploreText(w http.ResponseWriter, names []string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte("owners\n"))
	for _, n := range names {
		_, _ = w.Write([]byte(n + "\n"))
	}
}
