// search.go — the code-search API lanes (docs/features/20_search.md §6/§9/§10;
// docs/go/07_api.md §9.13). The service itself lives in internal/search and
// owns the query grammar, the gating order and the completeness accounting;
// this file decodes the wire knobs, preserves the frozen envelope and maps the
// service's classified failures onto the §10 table. It never invents a field
// and never answers 500: an unexpected failure is the documented 503.
package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/crueber/spindle/internal/search"
	"github.com/crueber/spindle/internal/server/auth"
)

// SearchService is the code-search seam (07_api.md §9.13). Implemented by
// *search.Service; composition binds it from collabWiring. A nil seam answers
// the documented 503 rather than an ungated search (fail closed, law 9).
type SearchService interface {
	Search(ctx context.Context, scope search.Scope, params search.Params, p auth.Principal) (*search.Answer, error)
}

// --- routes -------------------------------------------------------------------

// searchRepo is GET /{owner}/{repo}/api/search (and the api-browser twin).
// The dispatcher has already enforced AuthRead via the identity require_read
// hook, so the gate ran BEFORE query validation, existence or ref work (§8).
func (h *handlers) searchRepo(w http.ResponseWriter, r *http.Request) {
	h.search(w, r, search.Scope{
		Kind:  search.ScopeRepo,
		Owner: r.PathValue("owner"),
		Repo:  r.PathValue("repo"),
	})
}

// searchOwner is GET /api/v1/search (and /api-browser/v1/search). Owner scope
// is not repo-scoped, so it carries the ordinary AuthRead gate here; the
// service then filters candidates through the same read authority and a denied
// repository contributes nothing (no name, no count, no reason — §8 leak
// rules). Without an owner the service answers the 400 that names the rule.
func (h *handlers) searchOwner(w http.ResponseWriter, r *http.Request) {
	if !h.env.gate(w, r, AuthRead) {
		return
	}
	h.search(w, r, search.Scope{
		Kind:  search.ScopeOwner,
		Owner: strings.TrimSpace(r.URL.Query().Get("owner")),
	})
}

// search is the one handler body both routes share: no-store on every
// response (including errors — §6.1 says every response), decode, call, map.
func (h *handlers) search(w http.ResponseWriter, r *http.Request, scope search.Scope) {
	// The result is principal-, ref- and query-dependent: it must never be
	// revalidated or shared across principals (§6.1). Set before any error
	// path so the header rides errors too.
	w.Header().Set("Cache-Control", ccNoStore)
	w.Header().Del("ETag")

	if h.env.Search == nil {
		writePlain(w, http.StatusServiceUnavailable, search.ErrUnavailable.Error())
		return
	}
	params, err := decodeSearchParams(r)
	if err != nil {
		writeSearchErr(w, err)
		return
	}
	ans, err := h.env.Search.Search(r.Context(), scope, params, h.env.PrincipalOf(r))
	if err != nil {
		writeSearchErr(w, err)
		return
	}
	writeCached(w, r, ccNoStore, "", http.StatusOK, ans)
}

// --- request decoding ---------------------------------------------------------

// decodeSearchParams projects the frozen query knobs (§5.1, §9.13) onto the
// service's Params. It decodes but does not validate: text, paths, ref and
// type rules belong to search.Parse so every lane reads the same sentences.
// The knobs the service cannot see — the 0|1 switches and a non-integer
// limit — are shape errors here, with the same 400 class.
func decodeSearchParams(r *http.Request) (search.Params, error) {
	q := r.URL.Query()
	var p search.Params
	p.Text = q.Get("q")
	p.Type = q.Get("type")
	p.Ref = q.Get("ref")
	p.Paths = append([]string(nil), q["path"]...)

	var err error
	if p.Regex, err = decodeSwitch(q.Get("regex"), "regex"); err != nil {
		return search.Params{}, err
	}
	if p.Case, err = decodeSwitch(q.Get("case"), "case"); err != nil {
		return search.Params{}, err
	}
	if p.Word, err = decodeSwitch(q.Get("word"), "word"); err != nil {
		return search.Params{}, err
	}
	if p.Limit, err = decodeLimit(q.Get("limit")); err != nil {
		return search.Params{}, err
	}
	return p, nil
}

// decodeSwitch reads the 0|1 wire spelling of a boolean knob. Absent and "0"
// are false (the defaults in §5.1); anything else is a 400 naming the knob.
func decodeSwitch(raw, name string) (bool, error) {
	switch raw {
	case "", "0":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, &search.InvalidError{Field: name, Why: name + " must be 0 or 1"}
	}
}

// decodeLimit reads the optional match limit. Range clamping (1..200) is the
// service's; a non-integer is a shape error (§10 400-class).
func decodeLimit(raw string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, &search.InvalidError{Field: "limit", Why: "limit must be an integer"}
	}
	return n, nil
}

// --- failure mapping (§10) ----------------------------------------------------

// writeSearchErr renders the service's classified failures per §10. Every row
// is reachable here and nothing falls through to 500: the default is the
// documented "search is temporarily unavailable" 503.
func writeSearchErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, search.ErrInvalidQuery), errors.Is(err, search.ErrUnsupportedScope):
		writePlain(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, search.ErrNotFound):
		writePlain(w, http.StatusNotFound, err.Error())
	case errors.Is(err, search.ErrUnauthorized):
		writePlain(w, http.StatusUnauthorized, err.Error())
	case errors.Is(err, search.ErrForbidden):
		writePlain(w, http.StatusForbidden, err.Error())
	case errors.Is(err, search.ErrWarming):
		// §9: the serve bound fired; the caller retries in a few seconds
		// (five, not the generic 15).
		writeSearchRetry(w, err.Error(), "5")
	case errors.Is(err, search.ErrNotLocal),
		errors.Is(err, search.ErrDisabled),
		errors.Is(err, search.ErrIssuesUnavailable),
		errors.Is(err, search.ErrUnavailable):
		writePlain(w, http.StatusServiceUnavailable, err.Error())
	default:
		writePlain(w, http.StatusServiceUnavailable, search.ErrUnavailable.Error())
	}
}

// writeSearchRetry is a plain-text 503 with a search-specific Retry-After
// (writePlain's generic 15 is the wrong bound for a warming serve copy).
func writeSearchRetry(w http.ResponseWriter, msg, retryAfter string) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Del("ETag")
	h.Set("Retry-After", retryAfter)
	h.Set("Content-Length", itoa(len(msg)))
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(msg))
}
