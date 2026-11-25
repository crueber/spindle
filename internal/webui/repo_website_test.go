// repo_website_test.go — the About rail's Website row: only when set, a real
// link (new tab, noopener), escaped by html/template, with a truthful
// one-line display of a long URL.
package webui

import (
	"net/http"
	"strings"
	"testing"
)

func TestRepoAboutWebsiteLink(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t, true)
	f.view.summary.Website = "https://example.com/docs/start"
	body := f.mustGet("/demo/spindle", http.StatusOK)

	settingsMustContain(t, body, `<span class="kv-key">Website</span>`, "website row")
	settingsMustContain(t, body, `href="https://example.com/docs/start"`, "website href")
	settingsMustContain(t, body, `title="https://example.com/docs/start"`, "website title")
	settingsMustContain(t, body, `target="_blank"`, "new tab")
	settingsMustContain(t, body, `rel="noopener noreferrer nofollow"`, "link isolation")
	settingsMustContain(t, body, `>https://example.com/docs/start</a>`, "website text")
}

func TestRepoAboutWebsiteTruncatesLongURL(t *testing.T) {
	t.Parallel()
	long := "https://example.com/" + strings.Repeat("segment/", 8) + "index.html"
	if len(long) <= maxWebsiteDisplayRunes {
		t.Fatalf("fixture URL is not long enough: %d bytes", len(long))
	}
	f := newRepoFixture(t, true)
	f.view.summary.Website = long
	body := f.mustGet("/demo/spindle", http.StatusOK)

	// The link keeps the full URL; only the visible text is shortened.
	settingsMustContain(t, body, `href="`+long+`"`, "full href")
	settingsMustContain(t, body, `title="`+long+`"`, "full title")
	settingsMustContain(t, body, "…</a>", "ellipsised text")
	want := websiteDisplay(long)
	if n := len([]rune(want)); n != maxWebsiteDisplayRunes {
		t.Fatalf("display = %d runes, want %d", n, maxWebsiteDisplayRunes)
	}
	settingsMustContain(t, body, ">"+want+"</a>", "truncated text")
}

func TestRepoAboutWebsiteAbsentWhenUnset(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t, true)
	body := f.mustGet("/demo/spindle", http.StatusOK)
	settingsMustNotContain(t, body, ">Website</span>", "no website row when unset")
	settingsMustNotContain(t, body, "example.com", "no placeholder link")
	// The rest of the About rail is untouched.
	for _, row := range []string{">Stars</span>", ">Forks</span>", ">Watching</span>",
		">Branches</span>", ">Tags</span>"} {
		settingsMustContain(t, body, row, "about row")
	}
}

func TestRepoAboutWebsiteEscapesQuotes(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t, true)
	f.view.summary.Website = `https://example.com/a"b`
	body := f.mustGet("/demo/spindle", http.StatusOK)
	if strings.Contains(body, `title="https://example.com/a"b"`) {
		t.Fatalf("raw quote broke the attribute: %s", body)
	}
	settingsMustContain(t, body, "&#34;", "escaped quote")
	// html/template's URL normalizer percent-encodes the quote in href; either
	// way it is not a literal attribute break.
	settingsMustContain(t, body, `title="https://example.com/a&#34;b"`, "escaped title")
}

func TestWebsiteDisplay(t *testing.T) {
	t.Parallel()
	if got := websiteDisplay(""); got != "" {
		t.Fatalf("empty = %q", got)
	}
	short := "https://example.com/"
	if got := websiteDisplay(short); got != short {
		t.Fatalf("short = %q", got)
	}
	exact := "https://example.com/" + strings.Repeat("a", maxWebsiteDisplayRunes-len("https://example.com/"))
	if got := websiteDisplay(exact); got != exact {
		t.Fatalf("exact-length value was truncated: %q", got)
	}
	long := exact + "more"
	got := websiteDisplay(long)
	if n := len([]rune(got)); n != maxWebsiteDisplayRunes {
		t.Fatalf("long = %d runes: %q", n, got)
	}
	if !strings.HasPrefix(got, "https://example.com/") || !strings.HasSuffix(got, "…") {
		t.Fatalf("long display lost its shape: %q", got)
	}
}
