package packages

import (
	"context"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/crueber/spindle/internal/identity"
)

// TestDockerPushPull is the real-client e2e (docs/features/14 §8): when a
// Docker daemon is reachable, push a local image through the registry and pull
// it back by digest. Absent docker (or no local image) the test skips, never
// fails — the same rule the git e2e uses for a missing `git replay`. Docker
// treats loopback registries as insecure, so the plain-HTTP httptest server is
// reachable without daemon configuration.
func TestDockerPushPull(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e tier: skipped in -short mode")
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker binary not present")
	}
	run := func(timeout time.Duration, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		out, err := exec.CommandContext(ctx, docker, args...).CombinedOutput()
		return string(out), err
	}
	if out, err := run(10*time.Second, "info", "--format", "{{.ServerVersion}}"); err != nil {
		t.Skipf("docker daemon unavailable: %v (%s)", err, strings.TrimSpace(out))
	}
	out, err := run(10*time.Second, "image", "ls", "-q")
	if err != nil || strings.TrimSpace(out) == "" {
		t.Skipf("no local docker image to push: %v", err)
	}
	image := strings.Fields(out)[0]

	// Isolated docker config so `docker login` never touches the operator's.
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	f := newFixture(t)
	// A credentialed registry (the production shape): the daemon logs in and
	// exchanges Basic for a scoped token.
	f.h.AuthRequired = true
	f.svc.MaxBlobBytes = 0     // 10 GiB default for real layers
	f.svc.MaxManifestBytes = 0 // 4 MiB default
	f.roles.roles["alice|acme/demo"] = identity.RoleWrite
	srv := httptest.NewServer(f.h)
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")
	ref := host + "/acme/demo:e2e"

	var cleanupRefs = []string{ref}
	defer func() {
		for _, r := range cleanupRefs {
			_, _ = run(20*time.Second, "rmi", r)
		}
	}()

	if out, err := run(20*time.Second, "login", host, "-u", "alice", "-p", "tok"); err != nil {
		t.Fatalf("docker login: %v (%s)", err, out)
	}
	if out, err := run(20*time.Second, "tag", image, ref); err != nil {
		t.Fatalf("docker tag: %v (%s)", err, out)
	}
	if out, err := run(120*time.Second, "push", ref); err != nil {
		t.Fatalf("docker push: %v (%s)", err, out)
	}
	// Tags list must show the pushed tag (one LIST, user-triggered).
	if w := f.as("reader", "GET", "/v2/acme/demo/tags/list", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "e2e") {
		t.Fatalf("tags list = %d %s", w.Code, w.Body.String())
	}
	// The registry's own digest (RepoDigests may still carry the image's
	// pre-existing upstream entry, which is not what we pushed).
	mw := f.as("reader", "GET", "/v2/acme/demo/manifests/e2e", nil)
	if mw.Code != 200 {
		t.Fatalf("pushed manifest = %d %s", mw.Code, mw.Body.String())
	}
	mdig := mw.Header().Get("Docker-Content-Digest")
	if !strings.HasPrefix(mdig, "sha256:") {
		t.Fatalf("missing Docker-Content-Digest: %q", mdig)
	}
	digestRef := host + "/acme/demo@" + mdig
	cleanupRefs = append(cleanupRefs, digestRef)
	if out, err := run(20*time.Second, "rmi", ref); err != nil {
		t.Fatalf("docker rmi local tag: %v (%s)", err, out)
	}
	if out, err := run(120*time.Second, "pull", digestRef); err != nil {
		t.Fatalf("docker pull by digest: %v (%s)", err, out)
	}
	// The registry contract, asserted where both image stores agree: the pulled
	// reference is a RepoDigest naming the pushed manifest, exactly the digest
	// the registry served. `{{.Id}}` is daemon bookkeeping and store-dependent
	// (classic graphdriver reports the config digest, the containerd image store
	// reports its own image ID), so comparing it across a pull asserts a Docker
	// internal, not our registry (the v0.8.1 tag CI failed that way on
	// ubuntu-latest). The ID comparison stays as an informational log only.
	repoDigests, err := run(20*time.Second, "image", "inspect", "--format", "{{range .RepoDigests}}{{println .}}{{end}}", digestRef)
	if err != nil {
		t.Fatalf("docker inspect pulled: %v (%s)", err, repoDigests)
	}
	wantDigestRef := host + "/acme/demo@" + mdig
	found := false
	for _, line := range strings.Split(repoDigests, "\n") {
		if strings.TrimSpace(line) == wantDigestRef {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("pulled image RepoDigests %q do not carry the served manifest %q", strings.TrimSpace(repoDigests), wantDigestRef)
	}
	pulled, err := run(20*time.Second, "image", "inspect", "--format", "{{.Id}}", digestRef)
	if err != nil {
		t.Fatalf("docker inspect pulled: %v (%s)", err, pulled)
	}
	original, err := run(20*time.Second, "image", "inspect", "--format", "{{.Id}}", image)
	if err != nil {
		t.Fatalf("docker inspect original: %v (%s)", err, original)
	}
	t.Logf("image id pulled %s, original %s (informational; store-dependent)", strings.TrimSpace(pulled), strings.TrimSpace(original))
}
