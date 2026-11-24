package webui

// fork_button_test.go — the masthead Fork control's markup contract.
//
// The count links to the forks list and the label starts a fork, and anchors
// cannot nest, so the group is ONE `.btn` shell holding two anchors. The shell
// drops `.btn`'s padding and flex gap; the anchors carry asymmetric padding so
// the rendered text reads like its Star/Watch siblings (`0 Fork`, not two
// floating controls), and each stays a >=24px hit target (WCAG 2.5.8).
//
// This test pins that anatomy: one shell, exactly two anchors, no text node
// between them (the gap is padding, not a space), the documented padding
// classes, and both destinations intact. A refactor that re-splits the control
// or re-adds per-anchor padding fails here instead of shipping a visual
// regression. The rendered proof lives in the style guideline (§3.1 masthead).

import (
	"net/http"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/crueber/spindle/internal/api"
)

func TestRepoHeaderForkButtonAnatomy(t *testing.T) {
	f := newRepoFixture(t, true)
	old := repoDeps
	t.Cleanup(func() { repoDeps = old })
	repoDeps = RepoDeps{
		ForkInfo: func(owner, repo string) (api.ForkSummary, bool) {
			return api.ForkSummary{Count: 7}, true
		},
	}
	body := f.mustGet("/demo/spindle", http.StatusOK)

	// Locate the group by its count destination: the shell is that anchor's
	// parent. This is stable regardless of sibling order.
	var count *html.Node
	walk(parsePage(t, body), func(n *html.Node) {
		if n.Data == "a" && attr(n, "href") == "/demo/spindle/forks" {
			count = n
		}
	})
	if count == nil {
		t.Fatal("masthead has no link to the forks list (/demo/spindle/forks)")
	}
	shell := count.Parent
	if shell == nil || shell.Type != html.ElementNode || shell.Data != "span" {
		t.Fatalf("fork count is not inside a <span> shell (parent = %v)", shell)
	}

	// One shell: the two anchors are the shell's only content. Whitespace-only
	// text nodes are not rendered between flex items (CSS Flexbox §4), so the
	// count/label gap must come from padding alone.
	var anchors []*html.Node
	for c := shell.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode && strings.TrimSpace(c.Data) == "" {
			continue
		}
		if c.Type != html.ElementNode || c.Data != "a" {
			t.Fatalf("fork shell holds a non-anchor child %q; the group must be exactly two anchors", nodeDescribe(c))
		}
		anchors = append(anchors, c)
	}
	if len(anchors) != 2 {
		t.Fatalf("fork shell anchor count = %d, want 2", len(anchors))
	}
	if anchors[0] != count {
		t.Errorf("count anchor is not first in the group")
	}

	// The shell is a .btn with .btn's padding and gap moved onto the anchors.
	for _, cls := range []string{"btn", "whitespace-nowrap", "p-0", "gap-0"} {
		mustHaveClass(t, shell, cls, "fork shell")
	}

	label := anchors[1]
	mustHaveAttr(t, count, "title", "View forks of demo/spindle")
	mustHaveAttr(t, label, "title", "Fork this repository")
	mustHaveAttr(t, label, "href", "/demo/spindle/fork")

	// Asymmetric padding: count = leading padding + the 6px count/label gap;
	// label = no leading padding + trailing padding. py-[3px] keeps each link
	// 24px tall inside the 26px shell; min-w-6 keeps the count 24px wide.
	for _, cls := range []string{"inline-flex", "items-center", "pl-2.5", "pr-1.5", "py-[3px]", "min-w-6", "hover:underline"} {
		mustHaveClass(t, count, cls, "fork count anchor")
	}
	for _, cls := range []string{"inline-flex", "items-center", "pl-0", "pr-2.5", "py-[3px]", "hover:underline"} {
		mustHaveClass(t, label, cls, "fork label anchor")
	}

	// The visible text is the count immediately followed by the label — the
	// ONLY separator is the count anchor's pr-1.5. No extra whitespace.
	if got := flexText(shell); got != "7Fork" {
		t.Errorf("fork group text = %q, want %q (count and label adjacent)", got, "7Fork")
	}
	if got := flexText(count); got != "7" {
		t.Errorf("count anchor text = %q, want %q", got, "7")
	}
	if got := flexText(label); got != "Fork" {
		t.Errorf("label anchor text = %q, want %q", got, "Fork")
	}

	// The count keeps its mono treatment, exactly like Star/Watch.
	mono := false
	walk(count, func(n *html.Node) {
		if n.Data == "span" && hasClass(n, "mono") {
			mono = true
		}
	})
	if !mono {
		t.Error("fork count lost its .mono span (Star/Watch keep theirs)")
	}

	// No nested anchors and no second tab-stop surprise.
	walk(shell, func(n *html.Node) {
		if n.Data == "a" && n != count && n != label {
			t.Errorf("fork group holds a third anchor (href %q)", attr(n, "href"))
		}
	})
	for _, a := range anchors {
		nested := false
		for c := a.FirstChild; c != nil; c = c.NextSibling {
			walk(c, func(n *html.Node) {
				if n.Data == "a" {
					nested = true
				}
			})
		}
		if nested {
			t.Errorf("anchor %q nests another anchor", attr(a, "href"))
		}
	}
}

// nodeDescribe renders a node for a failure message.
func nodeDescribe(n *html.Node) string {
	switch n.Type {
	case html.TextNode:
		return strings.TrimSpace(n.Data)
	default:
		return n.Data
	}
}

func mustHaveClass(t *testing.T, n *html.Node, cls, what string) {
	t.Helper()
	if !hasClass(n, cls) {
		t.Errorf("%s missing class %q (class = %q)", what, cls, attr(n, "class"))
	}
}

func mustHaveAttr(t *testing.T, n *html.Node, name, want string) {
	t.Helper()
	if got := attr(n, name); got != want {
		t.Errorf("%s %s = %q, want %q", nodeDescribe(n), name, got, want)
	}
}

// flexText is the text a reader sees inside a flex container: whitespace-only
// text nodes between flex items are dropped by the layout, so they are dropped
// here too (a literal space between the anchors contributes no gap).
func flexText(n *html.Node) string {
	var b strings.Builder
	var rec func(*html.Node)
	rec = func(x *html.Node) {
		if x.Type == html.TextNode {
			if strings.TrimSpace(x.Data) != "" {
				b.WriteString(x.Data)
			}
			return
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			rec(c)
		}
	}
	rec(n)
	return b.String()
}
