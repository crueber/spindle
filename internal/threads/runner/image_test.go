package runner

// Image-source policy tests (docs/threads.md §8.1/§8.6, M1 of
// docs/PLAN-ARTIFACT-KEEPER.md): the reference rewrite matrix, docker's
// --pull argv, the per-runner digest rule and the create-failure
// diagnostics. All of it is plan/argv, so no daemon is involved.

import (
	"strings"
	"testing"
)

// digest is a syntactically valid sha256 digest (the value is irrelevant).
const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestRewriteImageMatrix(t *testing.T) {
	for _, tc := range []struct {
		name string
		ref  string
		cfg  ImageConfig
		want string
	}{
		// No prefix: every reference is used exactly as written (today's
		// behaviour, the compatibility case).
		{"no prefix hub path", "alpine/git:latest", ImageConfig{}, "alpine/git:latest"},
		{"no prefix official", "golang:1.27", ImageConfig{}, "golang:1.27"},
		{"no prefix explicit hub", "docker.io/library/golang:1.27", ImageConfig{}, "docker.io/library/golang:1.27"},
		{"no prefix local", "localhost:5000/a/b@" + digest, ImageConfig{}, "localhost:5000/a/b@" + digest},
		{"no prefix ghcr", "ghcr.io/a/b:t", ImageConfig{}, "ghcr.io/a/b:t"},

		// A host-less reference is a Docker Hub reference and moves under the
		// prefix; the implicit "library/" namespace is NOT inserted (the
		// upstream proxy applies its own normalisation — see RewriteImage).
		{"hub path", "alpine/git:latest", ImageConfig{Prefix: "ak.example/dockerhub"}, "ak.example/dockerhub/alpine/git:latest"},
		{"official single segment", "golang:1.27", ImageConfig{Prefix: "ak.example/dockerhub"}, "ak.example/dockerhub/golang:1.27"},
		{"hub path bare", "alpine/git", ImageConfig{Prefix: "ak.example/dockerhub"}, "ak.example/dockerhub/alpine/git"},
		{"host-less digest", "golang@" + digest, ImageConfig{Prefix: "ak.example/dockerhub"}, "ak.example/dockerhub/golang@" + digest},

		// A reference that names a registry is left alone unless prefix-all.
		{"explicit hub untouched", "docker.io/library/golang:1.27", ImageConfig{Prefix: "ak.example/dockerhub"}, "docker.io/library/golang:1.27"},
		{"local untouched", "localhost:5000/a/b@" + digest, ImageConfig{Prefix: "ak.example/dockerhub"}, "localhost:5000/a/b@" + digest},
		{"ghcr untouched", "ghcr.io/a/b:t", ImageConfig{Prefix: "ak.example/dockerhub"}, "ghcr.io/a/b:t"},

		// Prefix-all replaces the registry host: the host is dropped (the
		// prefix names the mirror repository), never carried in the path.
		{"ghcr under prefix-all", "ghcr.io/a/b:t", ImageConfig{Prefix: "ak.example/ghcr", PrefixAll: true}, "ak.example/ghcr/a/b:t"},
		{"explicit hub under prefix-all", "docker.io/library/golang:1.27", ImageConfig{Prefix: "ak.example/dockerhub", PrefixAll: true}, "ak.example/dockerhub/library/golang:1.27"},
		{"user hub under prefix-all", "docker.io/alpine/git:latest", ImageConfig{Prefix: "ak.example/dockerhub", PrefixAll: true}, "ak.example/dockerhub/alpine/git:latest"},
		{"local under prefix-all", "localhost:5000/a/b@" + digest, ImageConfig{Prefix: "ak.example/dockerhub", PrefixAll: true}, "ak.example/dockerhub/a/b@" + digest},
		{"host-less under prefix-all", "golang:1.27", ImageConfig{Prefix: "ak.example/dockerhub", PrefixAll: true}, "ak.example/dockerhub/golang:1.27"},

		// Edges: empty stays empty, whitespace is trimmed, an IPv6-style
		// colon host keeps its brackets out of the tag split.
		{"empty", "", ImageConfig{Prefix: "ak.example/dockerhub"}, ""},
		{"trimmed", "  golang:1.27  ", ImageConfig{Prefix: "ak.example/dockerhub"}, "ak.example/dockerhub/golang:1.27"},
		{"bracketed host untouched", "[::1]:5000/a/b:t", ImageConfig{Prefix: "ak.example/dockerhub"}, "[::1]:5000/a/b:t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RewriteImage(tc.ref, tc.cfg); got != tc.want {
				t.Fatalf("RewriteImage(%q, %+v) = %q, want %q", tc.ref, tc.cfg, got, tc.want)
			}
		})
	}
}

func TestValidateImageConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  ImageConfig
		want string // fragment the error must carry; "" = valid
	}{
		{"off", ImageConfig{}, ""},
		{"plain prefix", ImageConfig{Prefix: "ak.example/dockerhub"}, ""},
		{"host and port", ImageConfig{Prefix: "localhost:5000/ci-images"}, ""},
		{"ipv4 port", ImageConfig{Prefix: "192.168.1.5:5000/a/b"}, ""},
		{"scheme", ImageConfig{Prefix: "https://ak.example/dockerhub"}, "--image-prefix"},
		{"trailing slash", ImageConfig{Prefix: "ak.example/dockerhub/"}, "--image-prefix"},
		{"no path", ImageConfig{Prefix: "ak.example"}, "--image-prefix"},
		{"uppercase", ImageConfig{Prefix: "AK.example/dockerhub"}, "--image-prefix"},
		{"whitespace", ImageConfig{Prefix: " ak.example/dockerhub"}, "--image-prefix"},
		{"prefix-all alone", ImageConfig{PrefixAll: true}, "--image-prefix-all"},
		{"bad pull", ImageConfig{Pull: "sometimes"}, "--image-pull"},
		{"pull always", ImageConfig{Pull: PullAlways}, ""},
		{"pull never", ImageConfig{Pull: PullNever}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateImageConfig(tc.cfg)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("ValidateImageConfig(%+v) = %v, want valid", tc.cfg, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateImageConfig(%+v) = %v, want an error naming %q", tc.cfg, err, tc.want)
			}
		})
	}
}

func TestBuildPlanRewritesImagesAndPull(t *testing.T) {
	job := fixtureJob()
	p, err := BuildPlan(PlanInput{
		Job: job, AttemptToken: "wjt_x",
		Images: ImageConfig{Prefix: "ak.example/dockerhub", Pull: PullAlways},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Image != "ak.example/dockerhub/golang:1.27" {
		t.Fatalf("job image = %q", p.Image)
	}
	if p.RequestedImage != "golang:1.27" {
		t.Fatalf("requested image = %q", p.RequestedImage)
	}
	containerArgv := strings.Join(p.ContainerArgv, " ")
	if !strings.Contains(containerArgv, "--pull=always") {
		t.Fatalf("container argv lacks the pull policy: %s", containerArgv)
	}
	if !strings.Contains(containerArgv, " ak.example/dockerhub/golang:1.27 -c ") {
		t.Fatalf("container argv did not rewrite the image: %s", containerArgv)
	}
	if len(p.Services) != 1 || p.Services[0].Image != "ak.example/dockerhub/redis:7" {
		t.Fatalf("service image = %+v", p.Services)
	}
	svcArgv := strings.Join(p.Services[0].Argv, " ")
	if !strings.Contains(svcArgv, "--pull=always") || !strings.Contains(svcArgv, " ak.example/dockerhub/redis:7") {
		t.Fatalf("service argv = %s", svcArgv)
	}
	// Credentials are never argv: the policy adds no flag that could carry one.
	for _, argv := range []string{containerArgv, svcArgv} {
		for _, forbidden := range []string{"password", "DOCKER_CONFIG", "--auth", "token"} {
			if strings.Contains(strings.ToLower(argv), strings.ToLower(forbidden)) {
				t.Fatalf("argv carries %q: %s", forbidden, argv)
			}
		}
	}
}

func TestBuildPlanDefaultPullIsMissing(t *testing.T) {
	p, err := BuildPlan(PlanInput{Job: fixtureJob(), AttemptToken: "wjt_x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(p.ContainerArgv, " "), "--pull=missing") {
		t.Fatalf("default argv = %v", p.ContainerArgv)
	}
}

func TestBuildPlanRewritesTheRunnerDefaultImage(t *testing.T) {
	job := fixtureJob()
	job.Spec.Container = nil // the workflow names no image: --image applies
	p, err := BuildPlan(PlanInput{
		Job: job, AttemptToken: "wjt_x", DefaultImage: "alpine/git:latest",
		Images: ImageConfig{Prefix: "ak.example/dockerhub"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Image != "ak.example/dockerhub/alpine/git:latest" || p.RequestedImage != "alpine/git:latest" {
		t.Fatalf("default image = %q (requested %q)", p.Image, p.RequestedImage)
	}
}

func TestBuildPlanRequireDigest(t *testing.T) {
	cfg := ImageConfig{RequireDigest: true}
	// A tag is refused, naming the image and the flag.
	_, err := BuildPlan(PlanInput{Job: fixtureJob(), AttemptToken: "wjt_x", Images: cfg})
	if err == nil || !strings.Contains(err.Error(), "--image-require-digest") || !strings.Contains(err.Error(), "golang:1.27") {
		t.Fatalf("tag image under require-digest = %v", err)
	}
	// A pinned job image passes; a tag-named service still refuses.
	job := fixtureJob()
	job.Spec.Container.Image = "golang@" + digest
	_, err = BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_x", Images: cfg})
	if err == nil || !strings.Contains(err.Error(), `service "redis"`) {
		t.Fatalf("service under require-digest = %v", err)
	}
	job.Spec.Services = nil
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_x", Images: cfg})
	if err != nil {
		t.Fatalf("digest-pinned image refused: %v", err)
	}
	if p.Image != "golang@"+digest {
		t.Fatalf("image = %q", p.Image)
	}
}

func TestHasImageDigest(t *testing.T) {
	for _, tc := range []struct {
		ref  string
		want bool
	}{
		{"golang@" + digest, true},
		{"docker.io/library/golang@" + digest, true},
		{"golang:1.27", false},
		{"golang@sha256:short", false},
		{"golang@sha256:0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef", false},
		{"golang@sha512:" + strings.Repeat("a", 64), false},
		{"golang", false},
	} {
		if got := HasImageDigest(tc.ref); got != tc.want {
			t.Fatalf("HasImageDigest(%q) = %v, want %v", tc.ref, got, tc.want)
		}
	}
}

func TestNewRejectsImageFlagCombinations(t *testing.T) {
	base := Config{ServerURL: "http://127.0.0.1:1", Token: "wrt_x", AllowHostDocker: true, ImagePrefixAll: true}
	if _, err := New(base); err == nil || !strings.Contains(err.Error(), "--image-prefix-all") {
		t.Fatalf("New with --image-prefix-all alone = %v", err)
	}
	bad := base
	bad.ImagePrefixAll = false
	bad.ImagePrefix = "https://ak.example/dockerhub"
	if _, err := New(bad); err == nil || !strings.Contains(err.Error(), "--image-prefix") {
		t.Fatalf("New with a scheme prefix = %v", err)
	}
	ok := base
	ok.ImagePrefixAll = false
	ok.ImagePrefix = "ak.example/dockerhub"
	ok.ImagePull = PullAlways
	ok.ImageRequireDigest = true
	if _, err := New(ok); err != nil {
		t.Fatalf("New with a valid policy = %v", err)
	}
}

// The digest requirement and the pull policy are independent: a digest-pinned
// image is immutable, so the flag stays whatever the operator chose (never a
// silent rewrite of --image-pull). docs/threads.md §8.6 states the rule.
func TestRequireDigestKeepsThePullPolicy(t *testing.T) {
	job := fixtureJob()
	job.Spec.Container.Image = "golang@" + digest
	job.Spec.Services = nil
	p, err := BuildPlan(PlanInput{
		Job: job, AttemptToken: "wjt_x",
		Images: ImageConfig{RequireDigest: true, Pull: PullNever},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(p.ContainerArgv, " "), "--pull=never") {
		t.Fatalf("digest rule rewrote the pull policy: %v", p.ContainerArgv)
	}
}
