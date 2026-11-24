package runner

import (
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads"
)

// oidcJob is the fixture with a job OIDC request token attached.
func oidcJob() *threads.FetchResult {
	job := fixtureJob()
	job.IDToken = &threads.IDTokenRequest{
		RequestURL:   "https://spindle.example/api/v1/threads/jobs/jr_acme/attempt/2/id-token",
		RequestToken: "wot_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ExpiresAt:    "2026-11-20T14:00:00Z",
	}
	return job
}

func TestBuildPlanIDTokenRequestEnvironment(t *testing.T) {
	job := oidcJob()
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_test"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if p.IDTokenRequestToken != job.IDToken.RequestToken {
		t.Fatalf("plan token = %q", p.IDTokenRequestToken)
	}
	create := strings.Join(p.ContainerArgv, " ")
	if !strings.Contains(create, "--env SPINDLE_ID_TOKEN_REQUEST_URL="+job.IDToken.RequestURL) {
		t.Fatalf("container argv lacks the request URL: %s", create)
	}
	if !strings.Contains(create, "--env ACTIONS_ID_TOKEN_REQUEST_URL="+job.IDToken.RequestURL) {
		t.Fatalf("container argv lacks the GitHub alias URL: %s", create)
	}
	if strings.Contains(create, job.IDToken.RequestToken) {
		t.Fatal("request token leaked into the container create argv")
	}

	step := p.Steps[1]
	if !containsString(step.EnvFrom, "SPINDLE_ID_TOKEN_REQUEST_TOKEN") ||
		!containsString(step.EnvFrom, "ACTIONS_ID_TOKEN_REQUEST_TOKEN") {
		t.Fatalf("step EnvFrom = %v", step.EnvFrom)
	}
	argv := strings.Join(step.Argv, " ")
	if !strings.Contains(argv, "--env SPINDLE_ID_TOKEN_REQUEST_TOKEN ") {
		t.Fatalf("exec argv must name the token, not carry it: %s", argv)
	}
	if strings.Contains(argv, job.IDToken.RequestToken) {
		t.Fatal("request token leaked into docker exec argv")
	}
	// The value is supplied by the executor's environment.
	values := strings.Join(credentialEnv(p, step.EnvFrom), " ")
	if !strings.Contains(values, "SPINDLE_ID_TOKEN_REQUEST_TOKEN="+job.IDToken.RequestToken) ||
		!strings.Contains(values, "ACTIONS_ID_TOKEN_REQUEST_TOKEN="+job.IDToken.RequestToken) ||
		!strings.Contains(values, "SPINDLE_JOB_TOKEN=wjt_test") {
		t.Fatalf("credential env = %v", credentialEnv(p, step.EnvFrom))
	}
	// It is masked in the runner's own log stream.
	found := false
	for _, m := range p.Mask {
		if m == job.IDToken.RequestToken {
			found = true
		}
	}
	if !found {
		t.Fatalf("request token is not in the mask list: %v", p.Mask)
	}
}

func TestBuildPlanIDTokenAbsentWithoutRequest(t *testing.T) {
	job := fixtureJob()
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_test"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if p.IDTokenRequestToken != "" {
		t.Fatalf("plan token = %q", p.IDTokenRequestToken)
	}
	argv := strings.Join(p.ContainerArgv, " ")
	if strings.Contains(argv, "ID_TOKEN_REQUEST") {
		t.Fatalf("argv mentions the OIDC env without a token: %s", argv)
	}
	if containsString(p.Steps[1].EnvFrom, "SPINDLE_ID_TOKEN_REQUEST_TOKEN") {
		t.Fatalf("step EnvFrom = %v", p.Steps[1].EnvFrom)
	}
}

func TestBuildPlanIDTokenNotGivenToDockerActions(t *testing.T) {
	job := oidcJob()
	job.Spec.Steps = append(job.Spec.Steps, threads.StepSpec{Uses: "docker://alpine:3"})
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_test"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	dockerStep := p.Steps[len(p.Steps)-1]
	if dockerStep.Kind != "docker" {
		t.Fatalf("step kind = %q (%+v)", dockerStep.Kind, dockerStep.SkipReason)
	}
	if containsString(dockerStep.EnvFrom, "SPINDLE_ID_TOKEN_REQUEST_TOKEN") ||
		containsString(dockerStep.EnvFrom, "ACTIONS_ID_TOKEN_REQUEST_TOKEN") ||
		containsString(dockerStep.EnvFrom, "SPINDLE_JOB_TOKEN") {
		t.Fatalf("docker action step got a credential by NAME: %v", dockerStep.EnvFrom)
	}
	if strings.Contains(strings.Join(dockerStep.Argv, " "), job.IDToken.RequestToken) {
		t.Fatal("docker action argv carries the request token")
	}
	if strings.Contains(strings.Join(dockerStep.Argv, " "), "ID_TOKEN_REQUEST") {
		t.Fatalf("docker action argv carries OIDC request material: %v", dockerStep.Argv)
	}
}

func TestCredentialEnvNilPlan(t *testing.T) {
	if got := credentialEnv(nil, []string{"SPINDLE_JOB_TOKEN"}); got != nil {
		t.Fatalf("credentialEnv(nil) = %v", got)
	}
}
