package runner

import (
	"strings"
	"testing"

	"github.com/crueber/spindle/internal/threads"
)

// Environment aliases and addresses (docs/features/21_parity_master.md PAR-110,
// PAR-111, PAR-114; docs/threads.md §3.5/§5.3). The token aliases are the SAME
// scoped attempt credential, delivered by name so no value reaches argv; the
// server/API addresses come from the configured external origin.

// envOf returns one name=value pair from a credential env slice.
func envOf(values []string, name string) (string, bool) {
	for _, kv := range values {
		if rest, ok := strings.CutPrefix(kv, name+"="); ok {
			return rest, true
		}
	}
	return "", false
}

// TestJobTokenAliasesReachEveryScriptStep pins PAR-110: SPINDLE_JOB_TOKEN,
// GITHUB_TOKEN and FORGEJO_TOKEN are delivered to every repository-code step,
// all carrying the one attempt credential, and none of them appears in argv.
func TestJobTokenAliasesReachEveryScriptStep(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_alias_token"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	for _, step := range p.Steps {
		if step.Kind == "docker" {
			t.Fatalf("fixture step %q unexpectedly docker", step.Name)
		}
		for _, name := range []string{"SPINDLE_JOB_TOKEN", "GITHUB_TOKEN", "FORGEJO_TOKEN"} {
			if !containsString(step.EnvFrom, name) {
				t.Fatalf("step %q does not receive %s by name: %v", step.Name, name, step.EnvFrom)
			}
		}
		argv := strings.Join(step.Argv, " ")
		if strings.Contains(argv, "wjt_alias_token") {
			t.Fatalf("the job token leaked into argv: %s", argv)
		}
		values := strings.Join(credentialEnv(p, step.EnvFrom, step.EnvSecret), " ")
		for _, name := range []string{"SPINDLE_JOB_TOKEN", "GITHUB_TOKEN", "FORGEJO_TOKEN"} {
			if got, ok := envOf(strings.Split(values, " "), name); !ok || got != "wjt_alias_token" {
				t.Fatalf("%s = %q (present %v), want the one attempt token", name, got, ok)
			}
		}
	}
	// Of the three bare --env names, at least GITHUB_TOKEN is present in argv
	// (bare: the value comes from the docker client's environment).
	if !strings.Contains(strings.Join(p.Steps[1].Argv, " "), "--env GITHUB_TOKEN") {
		t.Fatalf("GITHUB_TOKEN is not delivered by name: %v", p.Steps[1].Argv)
	}
}

// TestAuthoredTokenEnvBeatsTheAlias pins the layering: a workflow that sets
// GITHUB_TOKEN itself (publish-github.yml sets it to a PAT) keeps its value; the
// automatic alias must never overwrite it.
func TestAuthoredTokenEnvBeatsTheAlias(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{{
		Name: "publish", Run: "echo publish",
		Env: map[string]string{"GITHUB_TOKEN": "github_pat_value"},
	}}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_alias_token"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	step := p.Steps[0]
	if containsString(step.EnvFrom, "GITHUB_TOKEN") {
		t.Fatalf("the alias overrides an authored GITHUB_TOKEN: %v", step.EnvFrom)
	}
	argv := strings.Join(step.Argv, " ")
	if !strings.Contains(argv, "--env GITHUB_TOKEN=github_pat_value") {
		t.Fatalf("the authored value is not in argv: %s", argv)
	}
	// The other spellings still arrive as aliases.
	if !containsString(step.EnvFrom, "FORGEJO_TOKEN") || !containsString(step.EnvFrom, "SPINDLE_JOB_TOKEN") {
		t.Fatalf("unrelated aliases were suppressed: %v", step.EnvFrom)
	}
}

// TestSecretsGitHubTokenResolvesByName pins PAR-110's corpus form:
// `env: FORGEJO_TOKEN: ${{ secrets.GITHUB_TOKEN }}` resolves to the job token,
// delivered BY NAME (the value never becomes argv text).
func TestSecretsGitHubTokenResolvesByName(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{{
		Name: "git", Run: "git fetch", Shell: "sh",
		Env: map[string]string{"FORGEJO_TOKEN": "${{ secrets.GITHUB_TOKEN }}"},
	}}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_secret_alias"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	step := p.Steps[0]
	if got := step.EnvSecret["FORGEJO_TOKEN"]; got != "GITHUB_TOKEN" {
		t.Fatalf("EnvSecret = %v, want FORGEJO_TOKEN -> GITHUB_TOKEN", step.EnvSecret)
	}
	argv := strings.Join(step.Argv, " ")
	if strings.Contains(argv, "wjt_secret_alias") {
		t.Fatalf("the token leaked into argv: %s", argv)
	}
	if !strings.Contains(argv, "--env FORGEJO_TOKEN") {
		t.Fatalf("the variable is not delivered by name: %s", argv)
	}
	if got, ok := envOf(credentialEnv(p, step.EnvFrom, step.EnvSecret), "FORGEJO_TOKEN"); !ok || got != "wjt_secret_alias" {
		t.Fatalf("FORGEJO_TOKEN value = %q present=%v", got, ok)
	}
	// No duplicate delivery: the alias loop must not also add the name.
	count := 0
	for _, name := range step.EnvFrom {
		if name == "FORGEJO_TOKEN" {
			count++
		}
	}
	if count != 0 {
		t.Fatalf("FORGEJO_TOKEN is delivered twice: %v", step.EnvFrom)
	}
}

// TestRepositorySecretResolvesByNameOnly pins a non-token secret reference and
// the missing-secret rule (an EMPTY variable, as on GitHub, never an argv leak).
func TestRepositorySecretResolvesByNameOnly(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{{
		Name: "use", Run: "echo use", Shell: "sh",
		Env: map[string]string{"API_TOKEN": "${{ secrets.TOKEN }}", "MISSING": "${{ secrets.NOPE }}"},
	}}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_x"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	step := p.Steps[0]
	argv := strings.Join(step.Argv, " ")
	if strings.Contains(argv, "s3cr3t-value") {
		t.Fatalf("a repository secret leaked into argv: %s", argv)
	}
	env := credentialEnv(p, step.EnvFrom, step.EnvSecret)
	if got, ok := envOf(env, "API_TOKEN"); !ok || got != "s3cr3t-value" {
		t.Fatalf("API_TOKEN = %q present=%v", got, ok)
	}
	if got, ok := envOf(env, "MISSING"); !ok || got != "" {
		t.Fatalf("a missing secret must be an empty variable: %q present=%v", got, ok)
	}
}

// TestEmbeddedSecretReferenceIsSplicedAndMasked pins the execution-wave rule
// (docs/threads.md §3.6): an EMBEDDED `${{ secrets.X }}` is substituted like on
// GitHub — the value cannot stay out of argv there — and the plan registers it
// with the log mask so the stream still never prints it. A whole-value `env:`
// entry keeps the stronger by-NAME delivery (see the test above).
func TestEmbeddedSecretReferenceIsSplicedAndMasked(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{{
		Name: "use", Run: "echo use", Shell: "sh",
		Env: map[string]string{"URL": "https://user:${{ secrets.TOKEN }}@example.test"},
	}}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_x"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	step := p.Steps[0]
	if len(step.EnvSecret) != 0 {
		t.Fatalf("an embedded reference was treated as a whole-value secret: %v", step.EnvSecret)
	}
	argv := strings.Join(step.Argv, " ")
	if !strings.Contains(argv, "https://user:s3cr3t-value@example.test") {
		t.Fatalf("the embedded secret was not substituted: %s", argv)
	}
	if strings.Contains(argv, "${{ secrets.TOKEN }}") {
		t.Fatalf("the reference was left visible in argv: %s", argv)
	}
	// The substituted value is registered with the plan's mask list, so the log
	// stream masks it exactly like a secret delivered by name.
	if got := maskValues(argv, p.Mask); strings.Contains(got, "s3cr3t-value") {
		t.Fatalf("the substituted secret is not masked: %q", got)
	}
}

// TestReservedTokenSecretCannotShadowTheJobToken pins the reserved name: a
// stored repository secret called GITHUB_TOKEN is ignored, so the variable has
// exactly one value (the scoped job token), never two.
func TestReservedTokenSecretCannotShadowTheJobToken(t *testing.T) {
	job := fixtureJob()
	job.Spec.Services = nil
	job.Secrets["GITHUB_TOKEN"] = "shadow-value"
	job.Mask = append(job.Mask, "shadow-value")
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_reserved"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if _, ok := p.Secrets["GITHUB_TOKEN"]; ok {
		t.Fatalf("the reserved secret was delivered: %v", p.Secrets)
	}
	step := p.Steps[0]
	values := credentialEnv(p, step.EnvFrom, step.EnvSecret)
	seen := 0
	for _, kv := range values {
		if strings.HasPrefix(kv, "GITHUB_TOKEN=") {
			seen++
			if kv != "GITHUB_TOKEN=wjt_reserved" {
				t.Fatalf("GITHUB_TOKEN = %q, want the job token", kv)
			}
		}
	}
	if seen != 1 {
		t.Fatalf("GITHUB_TOKEN delivered %d times: %v", seen, values)
	}
	if strings.Contains(strings.Join(step.Argv, " "), "shadow-value") {
		t.Fatalf("the shadow value reached argv: %v", step.Argv)
	}
}

// TestMaskCoversTheJobToken pins masking: the alias value is masked in the log
// stream exactly like a repository secret (PAR-110's "masking must still work").
func TestMaskCoversTheJobToken(t *testing.T) {
	job := fixtureJob()
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "wjt_mask_me"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	found := false
	for _, v := range p.Mask {
		if v == "wjt_mask_me" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the attempt token is not masked: %v", p.Mask)
	}
	if got := maskValues("token is wjt_mask_me here", p.Mask); strings.Contains(got, "wjt_mask_me") {
		t.Fatalf("maskValues did not mask the token: %q", got)
	}
}

// TestServerURLsFromPayload pins PAR-111: the payload's canonical origin wins
// over the runner's --server (it is the address job containers can reach), the
// API URL is the REST root, and a trailing slash never reaches the environment.
func TestServerURLsFromPayload(t *testing.T) {
	job := fixtureJob()
	job.ServerURL = "https://git.example.com/"
	p, err := BuildPlan(PlanInput{Job: job, ServerURL: "http://127.0.0.1:8080"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	for name, want := range map[string]string{
		"GITHUB_SERVER_URL":  "https://git.example.com",
		"GITHUB_API_URL":     "https://git.example.com/api/v1",
		"FORGEJO_SERVER_URL": "https://git.example.com",
		"SPINDLE_SERVER":     "https://git.example.com",
	} {
		if got := p.Env[name]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	// The container env carries them (every step sees them), and no variable
	// holds a stray host.
	if got := p.ContainerEnv["GITHUB_SERVER_URL"]; got != "https://git.example.com" {
		t.Fatalf("container env GITHUB_SERVER_URL = %q", got)
	}
}

// TestServerURLsFallBackToTheRunner pins the fallback: with no payload origin
// the runner's --server is used, and a malformed value produces NO variables
// rather than a wrong one.
func TestServerURLsFallBackToTheRunner(t *testing.T) {
	job := fixtureJob()
	p, err := BuildPlan(PlanInput{Job: job, ServerURL: "http://spindle.internal:8088/"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if p.Env["GITHUB_SERVER_URL"] != "http://spindle.internal:8088" {
		t.Fatalf("fallback GITHUB_SERVER_URL = %q", p.Env["GITHUB_SERVER_URL"])
	}
	for _, bad := range []string{"", "not a url", "ftp://example.com", "example.com:8080"} {
		job := fixtureJob()
		job.ServerURL = bad
		pb, berr := BuildPlan(PlanInput{Job: job, ServerURL: bad})
		if berr != nil {
			t.Fatalf("BuildPlan(%q): %v", bad, berr)
		}
		if _, ok := pb.Env["GITHUB_SERVER_URL"]; ok {
			t.Fatalf("malformed origin %q produced GITHUB_SERVER_URL=%q", bad, pb.Env["GITHUB_SERVER_URL"])
		}
	}
}

// TestGitHubServerURLSubstitution pins the expression form: `${{ github.server_url }}`
// (and api_url) resolve to exactly the environment values, in both spacings.
func TestGitHubServerURLSubstitution(t *testing.T) {
	job := fixtureJob()
	job.ServerURL = "https://git.example.com"
	job.Spec.Services = nil
	job.Spec.Steps = []threads.StepSpec{
		{Name: "probe", Run: "curl ${{ github.server_url }}/${{ github.repository }}/x ${{github.api_url}}"},
	}
	p, err := BuildPlan(PlanInput{Job: job, AttemptToken: "t"})
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	run := p.Steps[0].Run
	if !strings.Contains(run, "https://git.example.com/") || !strings.Contains(run, "https://git.example.com/api/v1") {
		t.Fatalf("substitution = %q", run)
	}
	if strings.Contains(run, "${{ github.server_url }}") || strings.Contains(run, "${{github.api_url}}") {
		t.Fatalf("an address reference was left unresolved: %q", run)
	}
	if p.Env["GITHUB_SERVER_URL"] != "https://git.example.com" || p.Env["GITHUB_API_URL"] != "https://git.example.com/api/v1" {
		t.Fatalf("env does not match: %v", p.Env)
	}
}
