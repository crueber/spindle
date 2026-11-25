// repo_website_test.go — the About box's Website link: only when set, a real
// link whose visible text is the word "Website" (the full URL lives in
// href/title, never truncation), escaped by html/template.
package webui

import (
	"net/http"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// websiteAnchors returns the anchors whose visible text is exactly "Website".
func websiteAnchors(doc *html.Node) []*html.Node {
	var out []*html.Node
	walk(doc, func(n *html.Node) {
		if n.Data == "a" && strings.TrimSpace(nodeText(n)) == "Website" {
			out = append(out, n)
		}
	})
	return out
}

func TestRepoAboutWebsiteLink(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t, true)
	f.view.summary.Website = "https://example.com/docs/start?tab=readme&lang=en"
	body := f.mustGet("/demo/spindle", http.StatusOK)

	settingsMustNotContain(t, body, `<span class="kv-key">Website</span>`, "no website key row")
	settingsMustContain(t, body, `href="https://example.com/docs/start?tab=readme&amp;lang=en"`, "full website href")
	settingsMustContain(t, body, `title="https://example.com/docs/start?tab=readme&amp;lang=en"`, "full website title")
	settingsMustContain(t, body, `target="_blank"`, "new tab")
	settingsMustContain(t, body, `rel="noopener noreferrer nofollow"`, "link isolation")

	anchors := websiteAnchors(parsePage(t, body))
	if len(anchors) != 1 {
		t.Fatalf("Website anchors = %d, want 1", len(anchors))
	}
	if got := strings.TrimSpace(nodeText(anchors[0])); got != "Website" {
		t.Fatalf("visible text = %q, want Website", got)
	}
	// The link sits in the About box, above the key/value rows.
	about := strings.Index(body, ">About<")
	link := strings.Index(body, ">Website</a>")
	stars := strings.Index(body, ">Stars</span>")
	if about < 0 || link < 0 || stars < 0 || !(about < link && link < stars) {
		t.Errorf("Website link is not between the About heading and the rows: about=%d link=%d stars=%d", about, link, stars)
	}
}

func TestRepoAboutWebsiteEscapesQuotes(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t, true)
	f.view.summary.Website = `https://example.com/a"b`
	body := f.mustGet("/demo/spindle", http.StatusOK)

	if strings.Contains(body, `href="https://example.com/a"b"`) {
		t.Fatalf("raw quote broke the href attribute: %s", body)
	}
	if strings.Contains(body, `title="https://example.com/a"b"`) {
		t.Fatalf("raw quote broke the title attribute: %s", body)
	}
	// html/template's URL normalizer percent-encodes the quote in href; the
	// attribute escaper spells it &#34; in title.
	settingsMustContain(t, body, `href="https://example.com/a%22b"`, "escaped href")
	settingsMustContain(t, body, `title="https://example.com/a&#34;b"`, "escaped title")
}

func TestRepoAboutWebsiteNoTruncationForLongURL(t *testing.T) {
	t.Parallel()
	long := "https://example.com/" + strings.Repeat("segment/", 8) + "index.html"
	f := newRepoFixture(t, true)
	f.view.summary.Website = long
	body := f.mustGet("/demo/spindle", http.StatusOK)

	settingsMustContain(t, body, `href="`+long+`"`, "full href")
	settingsMustContain(t, body, `title="`+long+`"`, "full title")
	if n := len(websiteAnchors(parsePage(t, body))); n != 1 {
		t.Fatalf("Website anchors = %d, want 1", n)
	}
	// No ellipsised URL anywhere in the About box: the anchor text is the word.
	start, end := strings.Index(body, `>About<`), strings.Index(body, "</aside>")
	if start < 0 || end < start || strings.Contains(body[start:end], "…") {
		t.Error("a long URL must not produce an ellipsised label")
	}
}

func TestRepoAboutWebsiteAbsentWhenUnset(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t, true)
	body := f.mustGet("/demo/spindle", http.StatusOK)

	settingsMustNotContain(t, body, ">Website</a>", "no website link when unset")
	settingsMustNotContain(t, body, "example.com", "no placeholder link")
	// The rest of the About rail is untouched.
	for _, row := range []string{">Stars</span>", ">Forks</span>", ">Watching</span>",
		">Branches</span>", ">Tags</span>"} {
		settingsMustContain(t, body, row, "about row")
	}
}
