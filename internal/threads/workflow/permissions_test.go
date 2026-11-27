package workflow

import (
	"strings"
	"testing"
)

// TestPAR112PermissionScopeVocabulary pins the closed scope matrix (D-11):
// every supported scope parses, the underscore alias canonicalizes, and every
// scope this server cannot enforce is a named parse error — never inert.
func TestPAR112PermissionScopeVocabulary(t *testing.T) {
	accepted := []struct {
		name string
		src  string
		want map[string]string
	}{
		{
			name: "per-scope levels",
			src:  "contents: read\nissues: write\npull-requests: write\nid-token: write",
			want: map[string]string{"contents": "read", "issues": "write", "pull-requests": "write", "id-token": "write"},
		},
		{
			name: "underscore pull_requests alias canonicalizes",
			src:  "pull_requests: write",
			want: map[string]string{"pull-requests": "write"},
		},
		{
			name: "deployments scope (PAR-133)",
			src:  "deployments: write",
			want: map[string]string{"deployments": "write"},
		},
		{
			name: "explicit none is a denial",
			src:  "contents: none\nid-token: none",
			want: map[string]string{"contents": "none", "id-token": "none"},
		},
		{
			name: "write-all baseline",
			src:  "write-all",
			want: map[string]string{"all": "write-all"},
		},
		{
			name: "all key with per-scope override",
			src:  "all: read-all\ncontents: write",
			want: map[string]string{"all": "read-all", "contents": "write"},
		},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			w, err := Parse([]byte("on: push\npermissions:\n  " + strings.ReplaceAll(tc.src, "\n", "\n  ") +
				"\njobs:\n  b:\n    runs-on: linux\n    steps:\n      - run: \"true\"\n"))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			for k, want := range tc.want {
				if got := w.Permissions[k]; got != want {
					t.Fatalf("permissions[%q] = %q, want %q (all: %+v)", k, got, want, w.Permissions)
				}
			}
		})
	}

	refused := []struct {
		name string
		src  string
		want string
	}{
		{name: "packages has no job-token surface", src: "packages: write", want: `permissions.packages: scope "packages" is not implemented`},
		{name: "checks has a dedicated token", src: "checks: write", want: `scope "checks" is not implemented`},
		{name: "actions not a job capability", src: "actions: write", want: `scope "actions" is not implemented`},
		{name: "unknown scope", src: "contnets: read", want: `unknown scope "contnets"`},
		{name: "id-token read is invalid", src: "id-token: read", want: `level "read" is not valid for "id-token"`},
		{name: "bogus level", src: "contents: administrator", want: `level "administrator" is not valid for "contents"`},
		{name: "per-scope read-all", src: "contents: read-all", want: `only valid as the whole`},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte("on: push\npermissions:\n  " + tc.src +
				"\njobs:\n  b:\n    runs-on: linux\n    steps:\n      - run: \"true\"\n"))
			if err == nil {
				t.Fatalf("permissions %q parsed; want a named refusal", tc.src)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "supported: contents, deployments, id-token, issues, pull-requests") &&
				strings.Contains(tc.want, "not implemented") {
				t.Fatalf("error does not name the supported matrix: %v", err)
			}
		})
	}
}

// TestPAR119EnableOpenIDConnectKey pins the Forgejo spelling beside the
// permission spelling (PAR-119): both parse, at workflow and job level.
func TestPAR119EnableOpenIDConnectKey(t *testing.T) {
	src := `on: push
enable-openid-connect: true
permissions:
  id-token: write
jobs:
  deploy:
    runs-on: linux
    steps:
      - run: "true"
  other:
    runs-on: linux
    enable-openid-connect: true
    steps:
      - run: "true"
`
	w, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !w.EnableOpenIDConnect {
		t.Fatal("workflow-level enable-openid-connect was not carried")
	}
	if !w.Jobs["other"].EnableOpenIDConnect {
		t.Fatal("job-level enable-openid-connect was not carried")
	}
	if w.Jobs["deploy"].EnableOpenIDConnect {
		t.Fatal("deploy job did not declare the key but reads as enabled")
	}
	// A non-boolean value is a named parse error, not a truthy string.
	if _, err := Parse([]byte("on: push\nenable-openid-connect: yes-please\njobs:\n  b:\n    runs-on: linux\n    steps:\n      - run: \"true\"\n")); err == nil {
		t.Fatal("enable-openid-connect: <string> parsed")
	}
}
