package threads

// PAR-179 container-half unit coverage: policy validation, the deferred server
// check, and the runner-side volume/privilege grants.

import (
	"strings"
	"testing"
)

func TestValidateContainerPolicyKnobs(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy ContainerPolicy
		ok     bool
		want   string
	}{
		{"zero", ContainerPolicy{}, true, ""},
		{"named network", ContainerPolicy{Network: "spindle-jobs"}, true, ""},
		{"host", ContainerPolicy{Network: "host"}, false, "job isolation"},
		{"none", ContainerPolicy{Network: "none"}, false, "job isolation"},
		{"bridge", ContainerPolicy{Network: "bridge"}, false, "job isolation"},
		{"container namespace", ContainerPolicy{Network: "container:x"}, false, "host namespace"},
		{"leading dash", ContainerPolicy{Network: "-jobs"}, false, "valid docker network"},
		{"slash", ContainerPolicy{Network: "a/b"}, false, "valid docker network"},
		{"glob volume", ContainerPolicy{ValidVolumes: []string{"/srv/cache/*"}}, true, ""},
		{"relative volume", ContainerPolicy{ValidVolumes: []string{"srv/cache"}}, false, "absolute host path"},
		{"dotdot volume", ContainerPolicy{ValidVolumes: []string{"/srv/../etc"}}, false, "absolute host path"},
		{"unix socket", ContainerPolicy{DockerHost: "unix:///run/docker.sock"}, true, ""},
		{"tcp", ContainerPolicy{DockerHost: "tcp://dind:2375"}, true, ""},
		{"ssh", ContainerPolicy{DockerHost: "ssh://root@host"}, false, "unix:///absolute/socket"},
		{"relative socket", ContainerPolicy{DockerHost: "unix://run/docker.sock"}, false, "absolute socket path"},
		{"bad tcp", ContainerPolicy{DockerHost: "tcp://a b"}, false, "tcp:// takes host:port"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateContainerPolicy(tc.policy)
			if tc.ok {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestServerPolicyDefersGrantableShapes(t *testing.T) {
	pol := ServerContainerPolicy()
	// Grantable at creation: the runner decides (PAR-179/D-8).
	for _, tc := range []struct {
		name    string
		options string
		volumes []string
	}{
		{"privileged", "--privileged", nil},
		{"cap add", "--cap-add NET_ADMIN", nil},
		{"cap add inline", "--cap-add=ALL", nil},
		{"host volume", "", []string{"/srv/cache:/cache:ro"}},
		{"socket volume", "", []string{"/var/run/docker.sock:/var/run/docker.sock"}},
	} {
		if r := CheckWorkflowContainerOptionsWith(pol, "container", tc.options, tc.volumes); r != nil {
			t.Fatalf("%s was refused at creation: %s", tc.name, r.Reason)
		}
	}
	// Permanent refusals stay at the server.
	for _, tc := range []struct {
		name    string
		options string
		volumes []string
		want    string
	}{
		{"device", "--device /dev/sda", nil, "--device"},
		{"security opt", "--security-opt seccomp=unconfined", nil, "--security-opt"},
		{"host namespace", "--pid=host", nil, "--pid"},
		{"root volume", "", []string{"/:/host"}, "host root"},
		{"relative volume", "", []string{"./x:/x"}, "absolute host source"},
		{"bad volume", "", []string{"x"}, "expected source:target"},
		{"bad options", "", []string{"/a:/b:shared"}, "volume options"},
		{"cap without value", "--cap-add", nil, "without a capability"},
	} {
		r := CheckWorkflowContainerOptionsWith(pol, "container", tc.options, tc.volumes)
		if r == nil || !strings.Contains(r.Reason, tc.want) {
			t.Fatalf("%s: refusal = %+v, want %q", tc.name, r, tc.want)
		}
	}
}

func TestRunnerPolicyGrants(t *testing.T) {
	// The zero policy refuses privilege and every volume (today's behaviour).
	if r := CheckWorkflowContainerOptionsWith(ContainerPolicy{}, "container", "--privileged", nil); r == nil || !strings.Contains(r.Reason, "--container-privileged") {
		t.Fatalf("default privilege refusal = %+v", r)
	}
	if r := CheckWorkflowContainerOptionsWith(ContainerPolicy{}, "container", "", []string{"/srv/cache:/cache"}); r == nil || !strings.Contains(r.Reason, "--container-valid-volume") {
		t.Fatalf("default volume refusal = %+v", r)
	}
	// A granted runner admits only what its allowlist matches.
	granted := ContainerPolicy{Privileged: true, ValidVolumes: []string{"/srv/cache/*"}}
	if r := CheckWorkflowContainerOptionsWith(granted, "container", "--privileged --cap-add SYS_ADMIN", []string{"/srv/cache/a:/cache"}); r != nil {
		t.Fatalf("granted shapes refused: %s", r.Reason)
	}
	if r := CheckWorkflowContainerOptionsWith(granted, "container", "", []string{"/etc/passwd:/etc/passwd"}); r == nil || !strings.Contains(r.Reason, "not in the runner's") {
		t.Fatalf("outside the allowlist = %+v", r)
	}
	if r := CheckWorkflowContainerOptionsWith(granted, "container", "--privileged=true", nil); r == nil {
		t.Fatalf("a valued --privileged must be refused")
	}
	if r := CheckWorkflowContainerOptionsWith(ContainerPolicy{Privileged: true}, "container", "--cap-add", nil); r == nil {
		t.Fatalf("--cap-add without a value must be refused")
	}
}

func TestVolumeSpecParsing(t *testing.T) {
	for _, tc := range []struct {
		spec           string
		source, target string
		opts           string
		ok, optsOK     bool
	}{
		{"/a:/b", "/a", "/b", "", true, true},
		{"/a:/b:ro", "/a", "/b", "ro", true, true},
		{"/a:/b:ro,z", "/a", "/b", "ro,z", true, true},
		{"/a:/b:shared", "/a", "/b", "shared", true, false},
		{"x", "", "", "", false, true},
		{":/b", "", "", "", false, true},
	} {
		src, dst, opts, ok := splitVolumeSpec(tc.spec)
		if ok != tc.ok {
			t.Fatalf("%q ok = %v", tc.spec, ok)
		}
		if ok && (src != tc.source || dst != tc.target || opts != tc.opts) {
			t.Fatalf("%q = %q %q %q", tc.spec, src, dst, opts)
		}
		if got := validVolumeOptions(tc.opts); got != tc.optsOK {
			t.Fatalf("%q options valid = %v", tc.spec, got)
		}
	}
}
