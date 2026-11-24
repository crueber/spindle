package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The checkout failure contract (docs/threads.md §4.4): a fetch that cannot
// reach the origin must NOT end at git's bare `exit 128`. The script keeps
// git's output as the evidence and adds a named diagnostic that names the URL
// tried, the in-container reachability requirement and the setting that fixes
// it. These tests run the generated script under a real `sh` with a stub git,
// so the branch is exercised exactly as the job container runs it.

// stubGit writes a `git` executable whose fetch fails with output, and returns
// a PATH prefix holding it.
func stubGit(t *testing.T, output string, code int) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \" $* \" in\n" +
		"  *\" fetch \"*) printf '%s\\n' " + shQuote(output) + " >&2; exit " + strconv.Itoa(code) + " ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatalf("write stub git: %v", err)
	}
	return dir
}

// runCheckoutScript executes the generated script with the stub git on PATH and
// returns its combined output plus exit code.
func runCheckoutScript(t *testing.T, p *Plan, stubDir string) (string, int) {
	t.Helper()
	script := checkoutScript(p, "1")
	cmd := exec.Command("sh", "-e", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+stubDir+":"+os.Getenv("PATH"), "SPINDLE_JOB_TOKEN=wjt_test")
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run checkout script: %v\n%s", err, out)
		}
	}
	return string(out), code
}

func TestCheckoutScriptNamesAnUnreachableOrigin(t *testing.T) {
	p := &Plan{
		Workspace:         t.TempDir(),
		CheckoutURL:       "http://checkout.invalid:8080/acme/mono.git",
		CheckoutOriginURL: "http://127.0.0.1:8080/acme/mono.git",
		CheckoutSHA:       "cb38da1aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	stub := stubGit(t, "fatal: unable to access 'http://checkout.invalid:8080/acme/mono.git/': Could not resolve host: checkout.invalid", 128)
	out, code := runCheckoutScript(t, p, stub)
	if code != 128 {
		t.Fatalf("exit = %d, want git's 128\n%s", code, out)
	}
	// Git's own output stays: it is the evidence, not the whole message.
	if !strings.Contains(out, "Could not resolve host: checkout.invalid") {
		t.Fatalf("git's output was dropped:\n%s", out)
	}
	for _, want := range []string{
		"spindle: checkout failed for http://checkout.invalid:8080/acme/mono.git",
		"the payload named http://127.0.0.1:8080/acme/mono.git, rewritten for the container",
		"name or connection error",
		"must resolve and be reachable from INSIDE the job container",
		"server.public_url",
		"host.docker.internal",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("named checkout error misses %q:\n%s", want, out)
		}
	}
}

func TestCheckoutScriptExplainsNonNetworkFailures(t *testing.T) {
	p := &Plan{
		Workspace:   t.TempDir(),
		CheckoutURL: "http://spindle.example.com/acme/mono.git",
		CheckoutSHA: "cb38da1aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	stub := stubGit(t, "fatal: remote error: the job token has no push permission", 1)
	out, code := runCheckoutScript(t, p, stub)
	if code != 1 {
		t.Fatalf("exit = %d, want 1\n%s", code, out)
	}
	if !strings.Contains(out, "spindle: checkout failed for http://spindle.example.com/acme/mono.git") {
		t.Fatalf("no named checkout error:\n%s", out)
	}
	if strings.Contains(out, "name or connection error") {
		t.Fatalf("a non-network failure was labelled as unreachable:\n%s", out)
	}
	if !strings.Contains(out, "git's output above is the cause") {
		t.Fatalf("generic hint missing:\n%s", out)
	}
}

func TestCheckoutScriptIsValidShellAndSucceeds(t *testing.T) {
	p := &Plan{
		Workspace:   t.TempDir(),
		CheckoutURL: "http://spindle.example.com/acme/mono.git",
		CheckoutSHA: "cb38da1aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	stub := stubGit(t, "", 0)
	out, code := runCheckoutScript(t, p, stub)
	if code != 0 {
		t.Fatalf("success path exit = %d\n%s", code, out)
	}
	if !strings.Contains(out, "checked out cb38da1aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") {
		t.Fatalf("success path lost the checked-out line:\n%s", out)
	}
}

// TestCheckoutNoticeStatesTheResolvedBase pins the once-per-job start line: the
// URL tried plus the host:port it resolves to inside the container, and the
// payload URL when the loopback rewrite changed it.
func TestCheckoutNoticeStatesTheResolvedBase(t *testing.T) {
	p := &Plan{
		CheckoutURL:       "http://host.docker.internal:8088/acme/mono.git",
		CheckoutOriginURL: "http://127.0.0.1:8088/acme/mono.git",
	}
	got := checkoutNotice(p)
	for _, want := range []string{
		"checkout from http://host.docker.internal:8088/acme/mono.git",
		"reachable inside the job container as host.docker.internal:8088",
		"payload URL http://127.0.0.1:8088/acme/mono.git rewritten",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("checkoutNotice = %q, want %q", got, want)
		}
	}
	// A public URL is reported unchanged, with its own host:port.
	p = &Plan{CheckoutURL: "https://git.example.com/acme/mono.git", CheckoutOriginURL: "https://git.example.com/acme/mono.git"}
	got = checkoutNotice(p)
	if got != "checkout from https://git.example.com/acme/mono.git, reachable inside the job container as git.example.com" {
		t.Fatalf("public notice = %q", got)
	}
	// A URL with no host still states what was tried (no bogus endpoint).
	if got := checkoutNotice(&Plan{CheckoutURL: "mono.git"}); got != "checkout from mono.git" {
		t.Fatalf("hostless notice = %q", got)
	}
}
