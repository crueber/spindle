package webui

// header_icon_test.go — the site header's icon-control family.
//
// The theme menu, the create menu and the account menu are icon-only
// controls and therefore ONE component (`.icon-btn`, app.css §2 Controls): the
// same 32×32 box, border, radius, surface and hover. Before this test they were
// three different families — `.btn px-2 py-1` padded rectangles and a 32px
// circle with a ring — and the cluster read as "slightly off".
//
// This is a regression pin on the rendered markup (the way the a11y suite pins
// landmarks): add a fourth icon-only header control and it either uses
// `.icon-btn` or fails here.

import (
	"net/http"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/crueber/spindle/internal/server/auth"
)

func TestHeaderIconControlFamily(t *testing.T) {
	t.Parallel()
	f := newRepoFixture(t, true)
	f.principal = auth.Principal{Name: "alice", Admin: true}
	f.rebind()
	body := f.mustGet("/demo/spindle", http.StatusOK)
	root := parsePage(t, body)

	var header *html.Node
	walk(root, func(n *html.Node) {
		if n.Data == "header" {
			header = n
		}
	})
	if header == nil {
		t.Fatal("page has no <header> landmark")
	}

	// The three icon-only controls, found by their stable hooks/names.
	find := func(match func(*html.Node) bool) *html.Node {
		var out *html.Node
		walk(header, func(n *html.Node) {
			if out == nil && n.Data == "button" && match(n) {
				out = n
			}
		})
		return out
	}
	// The theme control is the menu trigger (PLAN-THEMES §7.4): the old
	// two-state toggle became a role="menu" radio group, so the button is
	// found by its accessible name.
	theme := find(func(n *html.Node) bool { return attr(n, "aria-label") == "Theme" })
	create := find(func(n *html.Node) bool { return attr(n, "aria-label") == "Create new" })
	account := find(func(n *html.Node) bool {
		return strings.HasPrefix(attr(n, "aria-label"), "Account menu for ")
	})
	for name, n := range map[string]*html.Node{"theme menu": theme, "create": create, "account": account} {
		if n == nil {
			t.Fatalf("header has no %s control", name)
		}
	}

	for name, n := range map[string]*html.Node{"theme menu": theme, "create": create, "account": account} {
		if !hasClass(n, "icon-btn") {
			t.Errorf("%s is not an .icon-btn (class = %q) — icon controls are one family", name, attr(n, "class"))
		}
		// The padded `.btn` family is for labelled controls only; mixing it back
		// in is the exact drift this test exists to catch.
		if hasClass(n, "btn") {
			t.Errorf("%s also carries .btn; icon controls use .icon-btn alone", name)
		}
		// One sizing model: the box, never per-control dimensions.
		for _, cls := range []string{"h-8", "w-8", "h-7", "w-7", "h-9", "w-9", "rounded-full", "bg-zinc-700"} {
			if hasClass(n, cls) {
				t.Errorf("%s carries per-control sizing/shape %q; the box lives in .icon-btn", name, cls)
			}
		}
	}

	// Accessible names are unchanged by the restyle.
	if attr(theme, "aria-label") != "Theme" {
		t.Errorf("theme menu accessible name = %q", attr(theme, "aria-label"))
	}
	if attr(theme, "title") != "Theme" {
		t.Errorf("theme menu title = %q", attr(theme, "title"))
	}
	if attr(theme, "aria-haspopup") != "menu" {
		t.Errorf("theme menu trigger aria-haspopup = %q, want menu", attr(theme, "aria-haspopup"))
	}
	if attr(theme, "aria-expanded") != "false" {
		t.Errorf("theme menu trigger must start collapsed, aria-expanded = %q", attr(theme, "aria-expanded"))
	}
	if attr(create, "title") != "Create new" {
		t.Errorf("create title = %q", attr(create, "title"))
	}
	if got := attr(account, "title"); got != "alice" {
		t.Errorf("account title = %q, want the principal name", got)
	}
	if got := strings.TrimSpace(flexText(account)); got != "A" {
		t.Errorf("account initials = %q, want %q", got, "A")
	}

	// The theme menu is a server-rendered radio group (PLAN-THEMES §7.4): the
	// panel exists with a name, and every row is a menuitemradio carrying its
	// theme key — the JS-free page is complete, the island only marks the
	// current choice. One row is "match system"; every registry key renders.
	var panel *html.Node
	var rows []*html.Node
	var themeMenu *html.Node
	walk(header, func(n *html.Node) {
		if hasAttr(n, "data-theme-menu") {
			themeMenu = n
		}
	})
	if themeMenu == nil {
		t.Fatal("header has no data-theme-menu root")
	}
	walk(themeMenu, func(n *html.Node) {
		if attr(n, "role") == "menu" {
			panel = n
		}
		if attr(n, "role") == "menuitemradio" {
			rows = append(rows, n)
		}
	})
	if panel == nil {
		t.Fatal("theme menu has no role=menu panel")
	}
	if attr(panel, "aria-label") != "Theme" {
		t.Errorf("theme menu panel name = %q, want Theme", attr(panel, "aria-label"))
	}
	if !hasAttr(panel, "hidden") {
		t.Error("theme menu panel must start hidden (the trigger opens it)")
	}
	keys := map[string]bool{}
	for _, n := range rows {
		keys[attr(n, "data-theme-option")] = true
		if attr(n, "aria-checked") == "" {
			t.Errorf("theme row %q has no aria-checked", attr(n, "data-theme-option"))
		}
	}
	for _, want := range []string{"system", "dark", "light"} {
		if !keys[want] {
			t.Errorf("theme menu has no %q row (keys = %v)", want, keys)
		}
	}

	// One glyph size across the family: 16px, the box's 7px inset.
	for name, n := range map[string]*html.Node{"theme menu": theme, "create": create} {
		var svg *html.Node
		walk(n, func(x *html.Node) {
			if svg == nil && x.Data == "svg" {
				svg = x
			}
		})
		if svg == nil {
			t.Fatalf("%s has no glyph", name)
		}
		if attr(svg, "width") != "16" || attr(svg, "height") != "16" {
			t.Errorf("%s glyph is %sx%s, want 16x16", name, attr(svg, "width"), attr(svg, "height"))
		}
		if attr(svg, "aria-hidden") != "true" {
			t.Errorf("%s glyph is not aria-hidden (the control owns the name)", name)
		}
	}
}

// hasAttr reports a present attribute, including a valueless one
// (`<div data-menu-panel role="menu" hidden>`), whose Val is "".
func hasAttr(n *html.Node, name string) bool {
	for _, a := range n.Attr {
		if a.Key == name {
			return true
		}
	}
	return false
}
