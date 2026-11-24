package webui

// settings_tab_partials_test.go — the settings-tab layout rule is structural,
// not a convention: every repo-settings tab body lives in exactly one
// partials/settings_repo_<tab>.html partial, and pages/settings/repo.html
// carries only the nav, the notices and the dispatch lines.
//
// Why a test: the single 1,266-line settings_repo.html collected three rebase
// conflicts in one day, and a stale branch's shell could silently drop another
// tab's dispatch line. A body added back to the shell (or a second define of
// the same body) must fail here, loudly, before it becomes a conflict.

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// settingsDispatchRe matches one canonical tab dispatch line.
var settingsDispatchRe = regexp.MustCompile(
	`^{{if eq \.Data\.Tab "([a-z0-9]+)"}}{{template "(settings-repo-[a-z0-9]+)" \.Data}}{{end}}$`)

// settingsDefineRe matches one template definition.
var settingsDefineRe = regexp.MustCompile(`{{define "([^"]+)"}}`)

func TestSettingsTabsArePartials(t *testing.T) {
	t.Parallel()
	s := newBare(t)
	tpl, ok := s.tpl["settings/repo"]
	if !ok {
		t.Fatal(`no "settings/repo" template set`)
	}

	// The tab inventory the nav renders (settingsRepoTabs is also the
	// settingsTabByID allow-list, so a tab missing here cannot route).
	navTabs := map[string]bool{}
	for _, tab := range settingsRepoTabs {
		navTabs["settings-repo-"+tab.ID] = true
	}

	// --- (a) every tab resolves to exactly one define ---------------------
	defines := map[string]int{}
	for _, tt := range tpl.Templates() {
		defines[tt.Name()]++
	}
	for name := range navTabs {
		if defines[name] != 1 {
			t.Errorf("template %q is defined %d times, want exactly 1", name, defines[name])
		}
	}
	for name, n := range defines {
		if strings.HasPrefix(name, "settings-repo-") && !navTabs[name] && name != "settings-repo-notwired" {
			t.Errorf("template %q has no nav tab (orphan define, defined %d times)", name, n)
		}
	}
	// The shared sub-templates are shared, not tab bodies: both must resolve.
	for _, name := range []string{
		"settings-repo-notwired",
		"settings-protection-list",
		"settings-protection-effective",
		"settings-protection-form",
	} {
		if defines[name] != 1 {
			t.Errorf("shared template %q is defined %d times, want exactly 1", name, defines[name])
		}
	}

	// --- the shell carries the dispatch, one line per nav tab -------------
	shell, err := fs.ReadFile(tplFS, "templates/pages/settings/repo.html")
	if err != nil {
		t.Fatalf("read shell: %v", err)
	}
	panel := settingsPanelBody(t, string(shell))
	dispatched := map[string]bool{}
	for _, line := range strings.Split(panel, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		m := settingsDispatchRe.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("#settings-panel carries non-dispatch content: %q", line)
			continue
		}
		if m[2] != "settings-repo-"+m[1] {
			t.Errorf("dispatch for tab %q names %q, want %q", m[1], m[2], "settings-repo-"+m[1])
		}
		if dispatched[m[2]] {
			t.Errorf("tab %q dispatched more than once", m[1])
		}
		dispatched[m[2]] = true
	}
	for name := range navTabs {
		if !dispatched[name] {
			t.Errorf("nav tab %q has no dispatch line in the shell", name)
		}
	}
	if len(dispatched) != len(navTabs) {
		t.Errorf("shell dispatches %d tabs, nav has %d", len(dispatched), len(navTabs))
	}

	// --- (b) the shell defines no body and holds no panel markup ----------
	shellDefines := settingsDefineRe.FindAllStringSubmatch(string(shell), -1)
	if len(shellDefines) != 1 || shellDefines[0][1] != "content" {
		for _, m := range shellDefines {
			t.Errorf("page shell defines template %q; bodies belong in partials", m[1])
		}
		if len(shellDefines) == 0 {
			t.Error(`page shell defines no "content" template`)
		}
	}

	// --- (c) one tab body per settings_repo_*.html file -------------------
	if _, err := fs.Stat(tplFS, "templates/partials/settings_repo.html"); err == nil {
		t.Error("partials/settings_repo.html still exists; tabs live in settings_repo_<tab>.html files")
	}
	files, err := fs.Glob(tplFS, "templates/partials/settings_repo_*.html")
	if err != nil || len(files) == 0 {
		t.Fatalf("settings tab partials: %v (%d)", err, len(files))
	}
	sharedHome := map[string]string{
		"settings-repo-notwired":        "settings_repo_notwired.html",
		"settings-protection-list":      "settings_repo_protection.html",
		"settings-protection-effective": "settings_repo_protection.html",
		"settings-protection-form":      "settings_repo_protection.html",
	}
	seenShared := map[string]bool{}
	for _, file := range files {
		body, err := fs.ReadFile(tplFS, file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		base := strings.TrimPrefix(file, "templates/partials/")
		tabDefines := 0
		for _, m := range settingsDefineRe.FindAllStringSubmatch(string(body), -1) {
			name := m[1]
			if home, shared := sharedHome[name]; shared {
				seenShared[name] = true
				if base != home {
					t.Errorf("%s defines shared template %q; it lives in %s", base, name, home)
				}
				continue
			}
			if strings.HasPrefix(name, "settings-repo-") {
				tabDefines++
				if want := "settings_repo_" + strings.TrimPrefix(name, "settings-repo-") + ".html"; base != want {
					t.Errorf("%s defines tab template %q; a tab body lives only in %s", base, name, want)
				}
				continue
			}
			t.Errorf("%s defines unexpected template %q", base, name)
		}
		if tabDefines > 1 {
			t.Errorf("%s defines %d tab bodies, want at most 1", base, tabDefines)
		}
	}
	for name := range sharedHome {
		if !seenShared[name] {
			t.Errorf("shared template %q is not defined in its partial", name)
		}
	}
}

// settingsPanelBody returns the text between <div id="settings-panel"> and its
// closing </div>. The panel holds dispatch lines only (the split's contract),
// so the first closing tag is its own.
func settingsPanelBody(t *testing.T, shell string) string {
	t.Helper()
	const open = `<div id="settings-panel">`
	i := strings.Index(shell, open)
	if i < 0 {
		t.Fatalf("shell has no %s", open)
	}
	rest := shell[i+len(open):]
	j := strings.Index(rest, "</div>")
	if j < 0 {
		t.Fatal("settings-panel div is not closed")
	}
	return rest[:j]
}
