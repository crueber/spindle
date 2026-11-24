package runner

// The named create-failure diagnostics (docs/threads.md §8.6): a mirror that
// is unreachable, one that refuses the credential, and one that has no such
// repository are three different operator mistakes, and docker's own sentence
// cannot tell them apart. Each case is driven through the fakedocker double.

import (
	"context"
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads"
)

// failingCreate runs one job whose container create fails with stderr detail.
func failingCreate(t *testing.T, detail string, images ImageConfig) (string, *threads.FetchResult) {
	t.Helper()
	job := fixtureJob()
	job.Spec.Services = nil // the subject is the job container's diagnostic
	docker := &fakeDocker{runOutputs: []runOutput{{
		match: "create --name sp-j-", errOut: detail, code: 125,
	}}}
	ex := executorWith(docker)
	ex.Images = images
	stream := &Streamer{}
	res := ex.Execute(context.Background(), ExecInput{Job: job, AttemptToken: "wjt_token_value"}, stream)
	if res.Conclusion != threads.JobFailure {
		t.Fatalf("conclusion = %s, want failure", res.Conclusion)
	}
	return stream.buf.String(), job
}

func TestImageCreateDiagnostics(t *testing.T) {
	mirror := ImageConfig{Prefix: "ak.example/dockerhub"}
	for _, tc := range []struct {
		name   string
		detail string
		cfg    ImageConfig
		want   []string
		absent []string
	}{
		{
			name:   "mirror unreachable",
			detail: `Error response from daemon: Get "https://ak.example/v2/": dial tcp 10.0.0.1:443: connect: connection refused`,
			cfg:    mirror,
			want: []string{
				"image pull failed for ak.example/dockerhub/golang:1.27",
				"ak.example did not answer",
				"mirror: --image-prefix ak.example/dockerhub rewrote golang:1.27 to ak.example/dockerhub/golang:1.27; there is no fallback to the public registry (fail closed)",
				"pull: missing",
				"the pull happens in the DOCKER DAEMON",
				"insecure-registries",
				"without --image-prefix",
			},
		},
		{
			name:   "credential refused",
			detail: `Error response from daemon: Head "https://ak.example/v2/dockerhub/golang/manifests/1.27": unauthorized: authentication required`,
			cfg:    mirror,
			want: []string{
				"image pull refused for ak.example/dockerhub/golang:1.27",
				"did not accept the credential (401)",
				"$DOCKER_CONFIG",
				"pull-only token for ak.example",
				"never in argv",
				"repository path produces this same message",
			},
		},
		{
			name:   "manifest unknown",
			detail: `Error response from daemon: manifest unknown`,
			cfg:    mirror,
			want: []string{
				"image not found: ak.example/dockerhub/golang:1.27",
				"answered 404",
				"the mirror must have the repository the prefix names",
			},
		},
		{
			name:   "no prefix points at the public registry",
			detail: `Error response from daemon: Get "https://registry-1.docker.io/v2/": net/http: request canceled while waiting for connection (Client.Timeout exceeded)`,
			cfg:    ImageConfig{},
			want: []string{
				"image pull failed for golang:1.27",
				"mirror: none (--image-prefix is off)",
				"the registry did not answer",
			},
			absent: []string{"--image-prefix ak.example", "no fallback"},
		},
		{
			name:   "pull always explains the cost",
			detail: `Error response from daemon: manifest unknown`,
			cfg:    ImageConfig{Prefix: "ak.example/dockerhub", Pull: PullAlways},
			want:   []string{"pull: always", "a registry outage fails even a cached image"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, _ := failingCreate(t, tc.detail, tc.cfg)
			for _, want := range tc.want {
				if !strings.Contains(log, want) {
					t.Fatalf("job log misses %q:\n%s", want, log)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(log, absent) {
					t.Fatalf("job log unexpectedly carries %q:\n%s", absent, log)
				}
			}
		})
	}
}

func TestImageCreateDiagnosticCoversAService(t *testing.T) {
	job := fixtureJob()
	docker := &fakeDocker{runOutputs: []runOutput{{
		match: "create --name sp-s-", errOut: `Error response from daemon: manifest unknown`, code: 125,
	}}}
	ex := executorWith(docker)
	ex.Images = ImageConfig{Prefix: "ak.example/dockerhub", PrefixAll: true}
	stream := &Streamer{}
	res := ex.Execute(context.Background(), ExecInput{Job: job, AttemptToken: "wjt_token_value"}, stream)
	if res.Conclusion != threads.JobFailure {
		t.Fatalf("conclusion = %s", res.Conclusion)
	}
	log := stream.buf.String()
	for _, want := range []string{
		"image not found: ak.example/dockerhub/redis:7",
		"rewrote redis:7 to ak.example/dockerhub/redis:7",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("job log misses %q:\n%s", want, log)
		}
	}
}

func TestUnknownCreateFailureGetsNoImageDiagnostic(t *testing.T) {
	log, _ := failingCreate(t, `Error response from daemon: invalid mount config for type "volume"`, ImageConfig{Prefix: "ak.example/dockerhub"})
	if strings.Contains(log, "image pull") || strings.Contains(log, "mirror:") {
		t.Fatalf("a non-image create failure got an image diagnosis:\n%s", log)
	}
}

func TestImageDiagnosticClassification(t *testing.T) {
	for _, tc := range []struct{ detail, want string }{
		{"dial tcp: lookup ak.example: no such host", imageFailureUnreachable},
		{"net/http: TLS handshake timeout", imageFailureUnreachable},
		{"http: server gave HTTP response to HTTPS client", imageFailureUnreachable},
		{"unauthorized: authentication required", imageFailureUnauthorized},
		{"pull access denied for x, repository does not exist", imageFailureUnauthorized},
		{"manifest unknown", imageFailureNotFound},
		{"NAME_UNKNOWN", imageFailureNotFound},
		{"invalid reference format", ""},
	} {
		if got := classifyImageFailure(tc.detail); got != tc.want {
			t.Fatalf("classifyImageFailure(%q) = %q, want %q", tc.detail, got, tc.want)
		}
	}
	// A plan-less diagnostic must not panic on a nil plan (defensive: the
	// helper is only called with a plan).
	if lines := imageDiagnostic(&Plan{}, "x:1", "x:1", "manifest unknown"); len(lines) == 0 {
		t.Fatal("no diagnostic lines")
	}
}

// A registry-named reference is not rewritten without --image-prefix-all, and
// --image-pull=never fails a cold image without a registry answer: both
// branches of the diagnostic must say so.
func TestImageDiagnosticUntouchedRefAndPullNever(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Container.Image = "ghcr.io/a/b:t"
	docker := &fakeDocker{runOutputs: []runOutput{{
		match: "create --name sp-j-", errOut: `Error response from daemon: manifest unknown`, code: 125,
	}}}
	ex := executorWith(docker)
	ex.Images = ImageConfig{Prefix: "ak.example/dockerhub", Pull: PullNever}
	stream := &Streamer{}
	if res := ex.Execute(context.Background(), ExecInput{Job: job, AttemptToken: "wjt_token_value"}, stream); res.Conclusion != threads.JobFailure {
		t.Fatalf("conclusion = %s", res.Conclusion)
	}
	log := stream.buf.String()
	for _, want := range []string{
		"image not found: ghcr.io/a/b:t",
		"names its own registry and was used as written (--image-prefix-all is off)",
		"pull: never",
		"only images already on the daemon run",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("job log misses %q:\n%s", want, log)
		}
	}
}
