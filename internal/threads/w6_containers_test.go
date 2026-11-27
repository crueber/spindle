package threads

// The job container's mapping keys at the SERVER boundary (PAR-077): the
// documented scalar-or-list spellings parse, and a key that is not applied is
// refused BY NAME at creation (credentials, a host-side port, a TTY option).
// The runner's argv is pinned in internal/threads/runner/container_keys_test.go.

import (
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads/workflow"
)

const containerKeysSource = `
name: keys
on: push
jobs:
  build:
    runs-on: linux
    container:
      image: alpine:3.20
      entrypoint: /bin/sh
      command: -c "sleep infinity"
      ports: 8080
    steps:
      - run: echo hi
`

// TestContainerMappingScalarSpellings: GitHub's documented scalar spelling of
// entrypoint/command/ports parses into the same fields the list form fills.
func TestContainerMappingScalarSpellings(t *testing.T) {
	w, err := workflow.Parse([]byte(containerKeysSource))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	job := w.Jobs["build"]
	if job.Container == nil {
		t.Fatal("no container")
	}
	if got := strings.Join(job.Container.Entrypoint, " "); got != "/bin/sh" {
		t.Fatalf("entrypoint = %q", got)
	}
	if got := strings.Join(job.Container.Command, "|"); got != "-c|sleep infinity" {
		t.Fatalf("command = %q", got)
	}
	if got := strings.Join(job.Container.Ports, "|"); got != "8080" {
		t.Fatalf("ports = %q", got)
	}
	// The list spelling keeps working, and a mapping credential is carried so
	// the creation gate can name it.
	w, err = workflow.Parse([]byte(strings.Replace(containerKeysSource,
		`      entrypoint: /bin/sh`,
		`      entrypoint: ["/bin/sh", "-e"]
      credentials:
        username: u
        password: p`, 1)))
	if err != nil {
		t.Fatalf("Parse with credentials: %v", err)
	}
	job = w.Jobs["build"]
	if got := strings.Join(job.Container.Entrypoint, " "); got != "/bin/sh -e" {
		t.Fatalf("list entrypoint = %q", got)
	}
}

// TestContainerCommandSpanRefused: `entrypoint`/`command` are applied literals,
// so a `${{ }}` span there is a parse error naming the field (D-16's rule) —
// never an argument that is the literal text of an expression.
func TestContainerCommandSpanRefused(t *testing.T) {
	src := strings.Replace(containerKeysSource,
		`      command: -c "sleep infinity"`,
		`      command: ["-c", "${{ secrets.CMD }}"]`, 1)
	if _, err := workflow.Parse([]byte(src)); err == nil || !strings.Contains(err.Error(), "container.command") {
		t.Fatalf("err = %v, want a container.command refusal", err)
	}
	// A literal containing `{` is unchanged.
	src = strings.Replace(containerKeysSource, `      command: -c "sleep infinity"`, `      command: ["-c", "echo {ok}"]`, 1)
	if _, err := workflow.Parse([]byte(src)); err != nil {
		t.Fatalf("a literal brace must parse: %v", err)
	}
}

// TestContainerCredentialsRefusedByCreationGate: the mapping parses, and the
// job is refused with a reason naming the field — never accepted-and-inert, and
// never a password in the reason.
func TestContainerCredentialsRefusedByCreationGate(t *testing.T) {
	w, err := workflow.Parse([]byte(strings.Replace(containerKeysSource,
		`      ports: 8080`,
		`      ports: 8080
      credentials:
        username: registry-user
        password: registry-password`, 1)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	spec := buildSpec("keys", w, w.Jobs["build"], nil)
	if spec.Container == nil || spec.Container.Credentials == nil {
		t.Fatalf("credentials did not reach the spec: %+v", spec.Container)
	}
	r := containerOptionRefusalReason(spec)
	if r == nil || r.Field != "container.credentials" {
		t.Fatalf("refusal = %+v", r)
	}
	if !strings.Contains(r.Reason, "runner operator's own credential") {
		t.Fatalf("reason does not name the policy: %s", r.Reason)
	}
	for _, value := range []string{"registry-user", "registry-password"} {
		if strings.Contains(r.Reason, value) {
			t.Fatalf("the refusal echoed a credential: %s", r.Reason)
		}
	}
	if got := ContainerCredentialsRefusalReason(spec); got == "" {
		t.Fatal("the runner-facing helper must return the same refusal")
	}
}

// TestContainerPortsCreationGate: a host-side or malformed port is refused at
// creation with the reason the runner would give.
func TestContainerPortsCreationGate(t *testing.T) {
	for _, tc := range []struct {
		ports string
		want  string
	}{
		{"[8080:80]", "container port only"},
		{"[notaport]", "container port number"},
	} {
		w, err := workflow.Parse([]byte(strings.Replace(containerKeysSource,
			`      ports: 8080`, `      ports: `+tc.ports, 1)))
		if err != nil {
			t.Fatalf("%s: Parse: %v", tc.ports, err)
		}
		spec := buildSpec("keys", w, w.Jobs["build"], nil)
		r := containerOptionRefusalReason(spec)
		if r == nil || r.Field != "container.ports" || !strings.Contains(r.Reason, tc.want) {
			t.Fatalf("%s: refusal = %+v", tc.ports, r)
		}
	}
}

// TestContainerTTYOptionCreationGate: `--tty` is refused by name at creation,
// with the in-container remedy (the runner's exec has no terminal).
func TestContainerTTYOptionCreationGate(t *testing.T) {
	r := CheckWorkflowContainerOptions("container", "--tty", nil)
	if r == nil || !strings.Contains(r.Reason, "no terminal") {
		t.Fatalf("refusal = %+v", r)
	}
}

// TestParseJobContainerPortTable pins the port parser's accepted and refused
// spellings, including the ephemeral loopback publish it produces.
func TestParseJobContainerPortTable(t *testing.T) {
	for _, tc := range []struct {
		spec  string
		port  string
		pub   string
		wantE string
	}{
		{spec: "8080", port: "8080", pub: "127.0.0.1::8080"},
		{spec: " 8080/tcp ", port: "8080/tcp", pub: "127.0.0.1::8080/tcp"},
		{spec: "53/udp", port: "53/udp", pub: "127.0.0.1::53/udp"},
		{spec: "1", port: "1", pub: "127.0.0.1::1"},
		{spec: "65535", port: "65535", pub: "127.0.0.1::65535"},
		{spec: "", wantE: "empty port"},
		{spec: "8080:80", wantE: "container port only"},
		{spec: "127.0.0.1:8080:80", wantE: "container port only"},
		{spec: "70000", wantE: "1…65535"},
		{spec: "0", wantE: "1…65535"},
		{spec: "-1", wantE: "1…65535"},
		{spec: "abc", wantE: "container port number"},
		{spec: "8080/sctp", wantE: "tcp or udp"},
		{spec: "8080/", wantE: "tcp or udp"},
	} {
		got, err := ParseJobContainerPort(tc.spec)
		if tc.wantE != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantE) {
				t.Fatalf("%q: err = %v, want %q", tc.spec, err, tc.wantE)
			}
			continue
		}
		if err != nil || got.Container != tc.port || got.Publish != tc.pub {
			t.Fatalf("%q = %+v (%v), want %q/%q", tc.spec, got, err, tc.port, tc.pub)
		}
	}
	// No credentials means no refusal, at either boundary.
	if got := ContainerCredentialsRefusalReason(nil); got != "" {
		t.Fatalf("nil spec = %q", got)
	}
	if got := ContainerCredentialsRefusalReason(&Spec{Container: &ContainerSpec{Image: "x"}}); got != "" {
		t.Fatalf("credential-free spec = %q", got)
	}
}
