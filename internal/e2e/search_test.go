// search_test.go — Feature 20 (docs/features/20_search.md) end to end: the
// whole feature on a real instance, driven exactly as it is wired.
//
//	one repository pushed with real git (a source file, a README and a binary
//	file carrying the same needle) → repo-scope API search (match path/line/
//	column, the frozen envelope, empty success, 400 bad regex, 404 unknown ref)
//	→ issue/PR search (a body marker and a PR title, matched where they live)
//	→ the moderation PUT hides the issue body and the hidden text can no longer
//	be matched or excerpted → a second, private repository is invisible to a
//	non-reader's owner-scope search (no name, no count) and visible again after
//	flipping visibility → both pages render the match escaped with a #L link,
//	and the private repository's page is the documented 404.
//
// Real server subprocess, real git binary, real OIDC bootstrap (oidp_test.go):
// alice is a host admin, bob a plain admitted user with no role on the private
// repository, so the visibility half is the identity gate's real answer and not
// a test double. Skipped in -short mode: this is the e2e tier (make e2e), not
// the fast tier (docs/go/15_testing.md §1).
//
// The push round-trip budget is the sim's assertion (internal/sim/budgets_test.go,
// law 6); search adds no push-path write, and the extra push after the searches
// below is the end-to-end check that a search never disturbs the push path.
package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

const (
	// searchNeedle is the fixed string in the source file, the README and the
	// binary blob. It appears at a pinned line/column in each text file.
	searchNeedle = "spindle-needle-4f7c19"
	// searchIssueMarker lives only in an issue BODY: hiding that body must be
	// the only thing standing between the marker and a match.
	searchIssueMarker = "search-issue-body-marker-a1b2"
	// searchPRMarker lives in a pull-request TITLE (metadata, not a body).
	searchPRMarker = "search-pr-title-marker-c3d4"
)

// searchResp is one raw response from the search API or a page.
type searchResp struct {
	status int
	header http.Header
	body   []byte
}

// searchGET issues one search-lane GET with an optional bearer token. Nothing
// follows redirects: a search answer is the assertion, not navigation.
func searchGET(t *testing.T, rawURL, token string) searchResp {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", rawURL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return searchResp{status: resp.StatusCode, header: resp.Header, body: body}
}

// searchAnswer is the frozen §6.2 envelope, decoded for assertions. An omitted
// section stays nil (never an empty struct), matching the wire.
type searchAnswer struct {
	Query   string `json:"query"`
	Type    string `json:"type"`
	Options struct {
		Regex         bool     `json:"regex"`
		CaseSensitive bool     `json:"case_sensitive"`
		Word          bool     `json:"word"`
		Paths         []string `json:"paths"`
		Limit         int      `json:"limit"`
	} `json:"options"`
	Scope struct {
		Kind  string `json:"kind"`
		Owner string `json:"owner"`
		Repo  string `json:"repo"`
	} `json:"scope"`
	Code *struct {
		Complete        bool `json:"complete"`
		ReposConsidered int  `json:"repos_considered"`
		ReposSearched   int  `json:"repos_searched"`
		Results         []struct {
			Repo       string `json:"repo"`
			Ref        string `json:"ref"`
			SHA        string `json:"sha"`
			Status     string `json:"status"`
			Reason     string `json:"reason"`
			Complete   bool   `json:"complete"`
			Truncated  bool   `json:"truncated"`
			TimedOut   bool   `json:"timed_out"`
			MatchCount int    `json:"match_count"`
			FileCount  int    `json:"file_count"`
			Files      []struct {
				Path    string `json:"path"`
				Matches []struct {
					Line   int    `json:"line"`
					Column int    `json:"column"`
					Text   string `json:"text"`
				} `json:"matches"`
			} `json:"files"`
		} `json:"results"`
	} `json:"code"`
	Issues *struct {
		Complete bool   `json:"complete"`
		Reason   string `json:"reason"`
		Scanned  struct {
			Threads       int  `json:"threads"`
			Events        int  `json:"events"`
			IndexComplete bool `json:"index_complete"`
		} `json:"scanned"`
		Hits []struct {
			Repo      string   `json:"repo"`
			Num       int      `json:"num"`
			Kind      string   `json:"kind"`
			Title     string   `json:"title"`
			State     string   `json:"state"`
			Author    string   `json:"author"`
			Labels    []string `json:"labels"`
			UpdatedAt string   `json:"updated_at"`
			Matched   string   `json:"matched"`
			Excerpt   string   `json:"excerpt"`
			URL       string   `json:"url"`
		} `json:"hits"`
	} `json:"issues"`
}

// decodeSearch asserts a 200 and decodes the envelope; what names the case in
// every failure message.
func decodeSearch(t *testing.T, r searchResp, what string) searchAnswer {
	t.Helper()
	if r.status != http.StatusOK {
		t.Fatalf("%s: status = %d, want 200 (body %s)", what, r.status, r.body)
	}
	var ans searchAnswer
	if err := json.Unmarshal(r.body, &ans); err != nil {
		t.Fatalf("%s: decode envelope: %v\nbody: %s", what, err, r.body)
	}
	t.Logf("%-42s 200  %d bytes  Cache-Control=%q  ETag=%q",
		what, len(r.body), r.header.Get("Cache-Control"), r.header.Get("ETag"))
	return ans
}

// findSearchFile returns the file entry for path, failing when absent.
func findSearchFile(t *testing.T, files []struct {
	Path    string `json:"path"`
	Matches []struct {
		Line   int    `json:"line"`
		Column int    `json:"column"`
		Text   string `json:"text"`
	} `json:"matches"`
}, path string) int {
	t.Helper()
	for i, f := range files {
		if f.Path == path {
			return i
		}
	}
	var names []string
	for _, f := range files {
		names = append(names, f.Path)
	}
	t.Fatalf("search result has no %q (files: %v)", path, names)
	return -1
}

func TestE2E_Search(t *testing.T) {
	if testing.Short() {
		t.Skip("search scenario runs in the e2e tier (make e2e)")
	}
	requireModernGit(t)
	g := newGitClient(t)
	// chainServer mints alice (host admin) and bob (plain admitted user); bob
	// is the non-reader the private-repository half needs.
	s := chainServer(t)
	owner := testOwner

	repo := uniqueRepo("search")
	privateRepo := uniqueRepo("search-private")
	repoLane := lane(s, repo, "")

	// ---- seed one repository over real git -------------------------------------
	// main.go pins the needle at line 3, column 4 (after "// "); README.md at
	// line 3, column 1. The source line also carries markup so the page's
	// escaping is observable. The binary blob has a NUL in its first bytes, so
	// git treats it as binary and -I must skip it.
	work := filepath.Join(t.TempDir(), "work")
	g.initRepo(work)
	src := "package demo\n\n// " + searchNeedle + " <script>alert(1)</script>\nfunc main() {}\n"
	readme := "# Demo\n\n" + searchNeedle + " also appears in the README.\n"
	binaryBlob := "binary\x00" + searchNeedle + "\x00tail"
	g.commitFile(work, "main.go", src, "seed source")
	g.commitFile(work, "README.md", readme, "seed readme")
	g.commitFile(work, "binary.dat", binaryBlob, "seed binary")
	headSHA := g.run(work, "rev-parse", "refs/heads/main")
	g.run(work, "remote", "add", "origin", s.gitURL(owner, repo))
	g.run(work, "push", "-u", "origin", "main")
	// The PR below needs a head branch that is not the base.
	g.run(work, "checkout", "-b", "feature")
	g.commitFile(work, "feature.txt", "feature branch\n", "feature commit")
	g.run(work, "push", "origin", "feature")
	g.run(work, "checkout", "main")

	// Open it through the API first: the summary must show the push.
	var summary struct {
		FullName string  `json:"full_name"`
		Head     *apiRef `json:"head"`
	}
	getJSON(t, repoLane, &summary)
	if summary.Head == nil || summary.Head.SHA != headSHA {
		t.Fatalf("summary head = %+v, want %q", summary.Head, headSHA)
	}
	t.Logf("opened %s at %s", summary.FullName, headSHA)

	// ---- repository scope: the match, the envelope, the empty success ----------
	r := searchGET(t, repoLane+"/search?q="+url.QueryEscape(searchNeedle), "")
	ans := decodeSearch(t, r, "repo scope: needle")
	if cc := r.header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store (cachepolicy.NoStore)", cc)
	}
	if et := r.header.Get("ETag"); et != "" {
		t.Fatalf("search answered an ETag %q (no-store bodies never revalidate)", et)
	}
	if ans.Query != searchNeedle || ans.Type != "code" {
		t.Fatalf("query/type = %q/%q", ans.Query, ans.Type)
	}
	if ans.Options.Paths == nil || len(ans.Options.Paths) != 0 || ans.Options.Limit != 100 ||
		ans.Options.Regex || ans.Options.CaseSensitive || ans.Options.Word {
		t.Fatalf("options = %+v, want the normalised defaults", ans.Options)
	}
	if ans.Scope.Kind != "repo" || ans.Scope.Owner != owner || ans.Scope.Repo != repo {
		t.Fatalf("scope = %+v", ans.Scope)
	}
	if ans.Issues != nil {
		t.Fatal("a code search answered an issues section")
	}
	if ans.Code == nil || !ans.Code.Complete || ans.Code.ReposConsidered != 1 || ans.Code.ReposSearched != 1 {
		t.Fatalf("code section = %+v", ans.Code)
	}
	if len(ans.Code.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(ans.Code.Results))
	}
	res := ans.Code.Results[0]
	if res.Repo != owner+"/"+repo || res.Ref != "main" || res.SHA != headSHA {
		t.Fatalf("result repo/ref/sha = %q/%q/%q, want %q/main/%q", res.Repo, res.Ref, res.SHA, owner+"/"+repo, headSHA)
	}
	if res.Status != "ok" || !res.Complete || res.Truncated || res.TimedOut {
		t.Fatalf("result status/completeness = %q/%v/%v/%v", res.Status, res.Complete, res.Truncated, res.TimedOut)
	}
	if res.MatchCount != 2 || res.FileCount != 2 {
		t.Fatalf("match/file count = %d/%d, want 2/2 (the binary blob must be skipped)", res.MatchCount, res.FileCount)
	}
	srcIdx := findSearchFile(t, res.Files, "main.go")
	if m := res.Files[srcIdx].Matches[0]; m.Line != 3 || m.Column != 4 || !strings.Contains(m.Text, searchNeedle) {
		t.Fatalf("main.go match = %+v, want line 3 column 4", m)
	}
	readmeIdx := findSearchFile(t, res.Files, "README.md")
	if m := res.Files[readmeIdx].Matches[0]; m.Line != 3 || m.Column != 1 {
		t.Fatalf("README.md match = %+v, want line 3 column 1", m)
	}
	for _, f := range res.Files {
		if f.Path == "binary.dat" {
			t.Fatal("-I did not skip the binary blob")
		}
	}

	// A no-match query is a full, complete answer with an empty file list.
	r = searchGET(t, repoLane+"/search?q="+url.QueryEscape("search-no-such-token-zz"), "")
	ans = decodeSearch(t, r, "repo scope: no match")
	if ans.Code == nil || !ans.Code.Complete || len(ans.Code.Results) != 1 {
		t.Fatalf("no-match code section = %+v", ans.Code)
	}
	if nr := ans.Code.Results[0]; nr.MatchCount != 0 || nr.FileCount != 0 || !nr.Complete ||
		len(nr.Files) != 0 || nr.Files == nil {
		t.Fatalf("no-match result = %+v (files must be [] not null)", nr)
	}

	// A rejected pattern is a 400 with the §10 sentence, not an empty success.
	r = searchGET(t, repoLane+"/search?regex=1&q=%5B", "")
	if r.status != http.StatusBadRequest || !strings.Contains(string(r.body), "invalid regular expression") {
		t.Fatalf("bad regex = %d %q, want 400 naming the pattern", r.status, r.body)
	}
	t.Logf("bad regex                              400  %s", strings.TrimSpace(string(r.body)))

	// A well-formed but unknown ref is the 404 class.
	r = searchGET(t, repoLane+"/search?ref="+url.QueryEscape("no-such-branch")+"&q="+url.QueryEscape(searchNeedle), "")
	if r.status != http.StatusNotFound || !strings.Contains(string(r.body), "unknown revision") {
		t.Fatalf("unknown ref = %d %q, want 404 unknown revision", r.status, r.body)
	}
	t.Logf("unknown ref                            404  %s", strings.TrimSpace(string(r.body)))

	// ---- issue and pull-request search ----------------------------------------
	issueRes := mustAPI(t, "POST", repoLane+"/issues",
		`{"title":"An issue about searching","body":"the `+searchIssueMarker+` marker is only in this body"}`, chainAliceTok)
	issueNum := int(jget(t, issueRes, "thread", "num").(float64))
	prRes := mustAPI(t, "POST", repoLane+"/pulls",
		`{"title":"`+searchPRMarker+` in a title","base_ref":"refs/heads/main","head_ref":"refs/heads/feature","body":"no marker here"}`,
		chainAliceTok)
	prNum := int(jget(t, prRes, "thread", "num").(float64))
	t.Logf("created issue #%d and pull request #%d", issueNum, prNum)

	r = searchGET(t, repoLane+"/search?type=issues&q="+url.QueryEscape(searchIssueMarker), "")
	ans = decodeSearch(t, r, "issue search: body marker")
	if ans.Code != nil {
		t.Fatal("type=issues answered a code section")
	}
	if ans.Issues == nil || !ans.Issues.Complete || len(ans.Issues.Hits) != 1 {
		t.Fatalf("issue section = %+v", ans.Issues)
	}
	hit := ans.Issues.Hits[0]
	if hit.Kind != "issue" || hit.Num != issueNum || hit.Matched != "body" ||
		!strings.Contains(hit.Excerpt, searchIssueMarker) {
		t.Fatalf("issue hit = %+v, want kind issue #%d matched body with the marker excerpt", hit, issueNum)
	}
	if hit.URL != "/"+owner+"/"+repo+"/issues/"+fmt.Sprint(issueNum) {
		t.Fatalf("issue hit url = %q", hit.URL)
	}
	if hit.Labels == nil {
		t.Fatal("issue hit labels is null (the wire says [])")
	}

	r = searchGET(t, repoLane+"/search?type=issues&q="+url.QueryEscape(searchPRMarker), "")
	ans = decodeSearch(t, r, "issue search: PR title")
	if ans.Issues == nil || !ans.Issues.Complete || len(ans.Issues.Hits) != 1 {
		t.Fatalf("PR section = %+v", ans.Issues)
	}
	prHit := ans.Issues.Hits[0]
	if prHit.Kind != "pr" || prHit.Num != prNum || prHit.Matched != "title" ||
		!strings.Contains(prHit.Title, searchPRMarker) {
		t.Fatalf("PR hit = %+v, want kind pr #%d matched title", prHit, prNum)
	}
	if prHit.URL != "/"+owner+"/"+repo+"/pulls/"+fmt.Sprint(prNum) {
		t.Fatalf("PR hit url = %q", prHit.URL)
	}

	// ---- moderation hides the issue body: no match, no excerpt ----------------
	// The opened event is seq 0; the moderation PUT takes the whole document
	// with its current version. Hiding that event is the existing moderation
	// surface's job — search only reads the folded projection.
	doc := mustAPI(t, "GET", repoLane+"/moderation", "", chainAliceTok)
	version := int(doc["version"].(float64))
	target := fmt.Sprintf("thread:%06x:event:%012x", issueNum, 0)
	hideBody := fmt.Sprintf(`{"version":%d,"hidden":{%q:{"reason":"spam"}}}`, version, target)
	if st, data := apiCall(t, "PUT", repoLane+"/moderation", hideBody, chainAliceTok); st != http.StatusOK {
		t.Fatalf("PUT moderation hide: status %d, body %s", st, data)
	}
	t.Logf("hid %s (moderation version %d)", target, version)

	r = searchGET(t, repoLane+"/search?type=issues&q="+url.QueryEscape(searchIssueMarker), "")
	ans = decodeSearch(t, r, "issue search: after hiding the body")
	if ans.Issues == nil || !ans.Issues.Complete {
		t.Fatalf("hidden-body section = %+v", ans.Issues)
	}
	if len(ans.Issues.Hits) != 0 {
		t.Fatalf("hidden body still matched: %+v", ans.Issues.Hits)
	}
	if strings.Contains(string(r.body), `"excerpt"`) {
		t.Fatalf("a hidden body produced an excerpt: %s", r.body)
	}

	// ---- owner scope: a private repository is absent, then present ------------
	// The second repository carries the same needle; it is private to everyone
	// but its admins (alice), so bob's owner-scope answer must not name it,
	// count it or consider it.
	privateWork := filepath.Join(t.TempDir(), "private")
	g.initRepo(privateWork)
	g.commitFile(privateWork, "secret.go", "package secret\n\n// "+searchNeedle+"\n", "private seed")
	g.run(privateWork, "remote", "add", "origin", s.gitURL(owner, privateRepo))
	g.run(privateWork, "push", "-u", "origin", "main")

	access := mustAPI(t, "GET", lane(s, privateRepo, "access"), "", chainAliceTok)
	accessVer := int(access["version"].(float64))
	privateBody := fmt.Sprintf(`{"version":%d,"visibility":"private","role_bindings":[]}`, accessVer)
	if st, data := apiCall(t, "PUT", lane(s, privateRepo, "access"), privateBody, chainAliceTok); st != http.StatusOK {
		t.Fatalf("make %s private: status %d, body %s", privateRepo, st, data)
	}

	ownerURL := s.base + "/api/v1/search?owner=" + url.QueryEscape(owner) + "&type=code&q=" + url.QueryEscape(searchNeedle)
	r = searchGET(t, ownerURL, chainBobTok)
	ans = decodeSearch(t, r, "owner scope as bob: private hidden")
	if ans.Scope.Kind != "owner" || ans.Scope.Owner != owner || ans.Scope.Repo != "" {
		t.Fatalf("owner scope = %+v", ans.Scope)
	}
	if ans.Code == nil || !ans.Code.Complete {
		t.Fatalf("owner code section = %+v", ans.Code)
	}
	if ans.Code.ReposConsidered != 1 || ans.Code.ReposSearched != 1 || len(ans.Code.Results) != 1 {
		t.Fatalf("owner counts = %d/%d/%d, want 1/1/1 (the private repo is denied, not counted)",
			ans.Code.ReposConsidered, ans.Code.ReposSearched, len(ans.Code.Results))
	}
	if got := ans.Code.Results[0].Repo; got != owner+"/"+repo {
		t.Fatalf("owner result repo = %q, want the public %q", got, owner+"/"+repo)
	}
	if strings.Contains(string(r.body), privateRepo) {
		t.Fatalf("the private repository's name leaked into the owner answer: %s", r.body)
	}

	// Anonymous repository scope on the private repo is the real 401 (the git
	// convention that makes a client erase its credential).
	r = searchGET(t, lane(s, privateRepo, "search?q="+url.QueryEscape(searchNeedle)), "")
	if r.status != http.StatusUnauthorized {
		t.Fatalf("anonymous private repo = %d, want 401", r.status)
	}
	if got := r.header.Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
		t.Fatalf("WWW-Authenticate = %q, want a Bearer challenge", got)
	}
	t.Logf("anonymous private repo                 401  WWW-Authenticate=%q", r.header.Get("WWW-Authenticate"))

	// A signed-in caller without a read role is a 403, not a 404.
	r = searchGET(t, lane(s, privateRepo, "search?q="+url.QueryEscape(searchNeedle)), chainBobTok)
	if r.status != http.StatusForbidden {
		t.Fatalf("bob on private repo = %d, want 403 (body %s)", r.status, r.body)
	}
	t.Logf("bob on private repo                    403  %s", strings.TrimSpace(string(r.body)))

	// The page is the documented 404 for an invisible repository (never a 403
	// page; the URL has no privilege of its own).
	r = searchGET(t, s.base+"/"+owner+"/"+privateRepo+"/search?q="+url.QueryEscape(searchNeedle), "")
	if r.status != http.StatusNotFound {
		t.Fatalf("private repo page = %d, want the documented 404 (body %s)", r.status, r.body)
	}
	t.Logf("private repo page                      404")

	// Flip it back and the same caller's owner-scope answer gains it.
	access = mustAPI(t, "GET", lane(s, privateRepo, "access"), "", chainAliceTok)
	accessVer = int(access["version"].(float64))
	publicBody := fmt.Sprintf(`{"version":%d,"visibility":"public","role_bindings":[]}`, accessVer)
	if st, data := apiCall(t, "PUT", lane(s, privateRepo, "access"), publicBody, chainAliceTok); st != http.StatusOK {
		t.Fatalf("make %s public: status %d, body %s", privateRepo, st, data)
	}
	r = searchGET(t, ownerURL, chainBobTok)
	ans = decodeSearch(t, r, "owner scope as bob: public again")
	if ans.Code == nil || !ans.Code.Complete || ans.Code.ReposConsidered != 2 || ans.Code.ReposSearched != 2 {
		t.Fatalf("re-public owner counts = %+v, want 2/2", ans.Code)
	}
	foundPrivate := false
	for _, cr := range ans.Code.Results {
		if cr.Repo != owner+"/"+privateRepo {
			continue
		}
		foundPrivate = true
		if cr.Status != "ok" || cr.MatchCount != 1 || len(cr.Files) != 1 || cr.Files[0].Path != "secret.go" {
			t.Fatalf("re-public repository result = %+v", cr)
		}
	}
	if !foundPrivate {
		t.Fatalf("the now-public repository is absent from the owner answer: %+v", ans.Code.Results)
	}

	// ---- pages ----------------------------------------------------------------
	// The repo page is a native GET form; the result is file-grouped, the match
	// is marked, the link is anchored to the resolved sha, and the source line's
	// markup is escaped by the server renderer (the XSS boundary).
	r = searchGET(t, s.base+"/"+owner+"/"+repo+"/search?q="+url.QueryEscape(searchNeedle), "")
	if r.status != http.StatusOK {
		t.Fatalf("repo search page = %d, want 200 (body %s)", r.status, r.body)
	}
	page := string(r.body)
	for _, want := range []string{
		"main.go",
		"README.md",
		`href="/` + owner + `/` + repo + `/blob/` + headSHA + `/main.go#L3"`,
		"<mark",
		"&lt;script&gt;alert(1)&lt;/script&gt;",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("repo search page is missing %q", want)
		}
	}
	if strings.Contains(page, "<script>alert(1)</script>") {
		t.Fatal("the match line's markup was not escaped on the search page")
	}
	if strings.Contains(page, privateRepo) {
		t.Fatal("the repo page named another repository")
	}
	t.Logf("repo search page                       200  %d bytes", len(r.body))

	// The owner page renders the same anatomy across repositories, and the
	// owner-less entry point explains why v1 does not search instance-wide.
	r = searchGET(t, s.base+"/search?owner="+url.QueryEscape(owner)+"&q="+url.QueryEscape(searchNeedle), "")
	if r.status != http.StatusOK {
		t.Fatalf("owner search page = %d, want 200 (body %s)", r.status, r.body)
	}
	ownerPage := string(r.body)
	for _, want := range []string{owner + "/" + repo, owner + "/" + privateRepo, "main.go"} {
		if !strings.Contains(ownerPage, want) {
			t.Fatalf("owner search page is missing %q", want)
		}
	}
	t.Logf("owner search page                      200  %d bytes", len(r.body))

	r = searchGET(t, s.base+"/search", "")
	if r.status != http.StatusOK || !strings.Contains(string(r.body), "Version 1 searches one owner at a time") {
		t.Fatalf("owner-less search page = %d, want 200 with the v1 explanation", r.status)
	}
	t.Logf("owner-less search page                 200")

	// ---- a search never disturbs the push path ---------------------------------
	g.commitFile(work, "after.txt", "a commit after the searches\n", "post-search commit")
	g.run(work, "push", "origin", "main")
	after := g.run(work, "rev-parse", "refs/heads/main")
	if got := g.lsRemote(s.gitURL(owner, repo))["refs/heads/main"]; got != after {
		t.Fatalf("push after search: ls-remote main = %q, want %q", got, after)
	}
	t.Logf("post-search push landed at %s", after)
}
