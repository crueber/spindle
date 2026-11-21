package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/threads"
)

// Job planning: the pure spec → argv mapping (15 §4). Everything the executor
// runs is decided here, so the unit tests assert the exact docker argv without
// a daemon. Names are a hash of (repo, run, job, attempt): deterministic (so
// cleanup on any path needs nothing but the plan), collision-free across
// concurrent jobs, and different for a new attempt.

// DefaultWorkspace is the in-container workspace mount (15 §4: a per-job named
// volume at the workdir).
const DefaultWorkspace = "/spindle/workspace"

// DefaultImage is the job container when the workflow names none. It must
// carry git (the checkout runs inside the container), so the smallest stock
// image with a real git is alpine/git.
const DefaultImage = "alpine/git:latest"

// maxStepOutput bounds one log frame (the server's cap is 256 KiB).
const maxStepOutput = 32 << 10

// PlanInput is everything needed to plan one job.
type PlanInput struct {
	Job *threads.FetchResult
	// AttemptToken is the wjt_ credential (checkout password; read-only).
	AttemptToken string
	DefaultImage string
	WorkspaceDir string
	// ServerURL is the spindle origin the runner was configured with (exposed
	// as SPINDLE_SERVER; informational).
	ServerURL string
}

// Names are the docker object names of one job attempt.
type Names struct {
	Container string
	Network   string
	Volume    string
	Services  map[string]string // service id → container name
}

// Plan is one fully-resolved job.
type Plan struct {
	JobRef      string
	Attempt     int
	Names       Names
	Workspace   string
	Image       string
	CheckoutURL string
	CheckoutSHA string
	Timeout     time.Duration

	// ContainerArgv is `docker create …` minus the leading "docker".
	ContainerArgv []string
	// ContainerEnv is the job-level environment the container itself carries.
	ContainerEnv map[string]string
	// Services in declaration order (sorted by id).
	Services []ServicePlan
	Steps    []StepPlan
	// Secrets are exposed to steps by NAME (the value travels in the docker
	// client's environment, never in argv) and masked in the log stream.
	Secrets map[string]string
	Mask    []string
	// JobToken is the attempt credential every step receives as
	// $SPINDLE_JOB_TOKEN (the checkout clones with it; a `contents: write` job
	// may push with it — 15 §5).
	JobToken string
	// Matrix/Inputs feed `if`, substitution and the environment; kept for
	// reporting.
	Matrix map[string]string
	Inputs map[string]string
	Env    map[string]string
}

// ServicePlan is one sidecar.
type ServicePlan struct {
	ID   string
	Name string
	Argv []string // docker create … minus "docker"
	// ProbePorts are the container TCP ports the runner polls for readiness
	// (published on loopback ephemeral ports).
	ProbePorts []string
}

// StepPlan is one step's exec invocation.
type StepPlan struct {
	Index            int
	Name             string
	Kind             string // run | checkout | unsupported
	Uses             string
	If               string
	ContinueOnError  bool
	Timeout          time.Duration
	WorkingDirectory string
	Run              string
	// Argv is `docker exec …` minus the leading "docker".
	Argv []string
	// EnvFrom are variable names supplied via `--env NAME` (values come from
	// the docker client's environment).
	EnvFrom []string
	// SkipReason is set for an unsupported action (the step fails with it).
	SkipReason string
}

// BuildPlan validates the spec and resolves one job into docker argv.
func BuildPlan(in PlanInput) (*Plan, error) {
	job := in.Job
	if job == nil || job.Spec == nil {
		return nil, fmt.Errorf("job payload has no spec")
	}
	spec := job.Spec
	if spec.Version != threads.SpecVersion {
		return nil, fmt.Errorf("unsupported job spec version %d (runner speaks %d)", spec.Version, threads.SpecVersion)
	}
	if spec.Uses != "" {
		return nil, fmt.Errorf("reusable workflows (job uses: %s) are not supported in v1", spec.Uses)
	}
	image := in.DefaultImage
	if image == "" {
		image = DefaultImage
	}
	if spec.Container != nil && strings.TrimSpace(spec.Container.Image) != "" {
		image = strings.TrimSpace(spec.Container.Image)
	}
	ws := in.WorkspaceDir
	if ws == "" {
		ws = DefaultWorkspace
	}
	names := jobNames(job)
	matrix := spec.Matrix
	inputs := spec.Inputs
	buildEnv := builtinEnv(job, ws, in.ServerURL, matrix)
	for k, v := range spec.Env {
		buildEnv[k] = headerValue(v)
	}
	jobEnv := map[string]string{}
	for k, v := range buildEnv {
		jobEnv[k] = v
	}
	if spec.Container != nil {
		for k, v := range spec.Container.Env {
			jobEnv[k] = v
		}
	}
	secretEnv := map[string]string{}
	mask := make([]string, 0, len(job.Secrets))
	for name, value := range job.Secrets {
		secretEnv[name] = value
	}
	mask = append(mask, job.Mask...)
	sort.Strings(mask)

	p := &Plan{
		JobRef: job.JobRef, Attempt: job.Attempt,
		Names: names, Workspace: ws, Image: image,
		CheckoutSHA: job.Commit, Secrets: secretEnv, Mask: mask,
		JobToken: in.AttemptToken,
		Matrix:   matrix, Inputs: inputs, Env: jobEnv,
	}
	if job.Checkout != nil {
		p.CheckoutURL = rewriteForContainer(job.Checkout.URL)
	}
	if spec.TimeoutMinutes > 0 {
		p.Timeout = time.Duration(spec.TimeoutMinutes) * time.Minute
	}
	p.ContainerEnv = jobEnv
	p.ContainerArgv = containerArgv(p, spec)

	ids := make([]string, 0, len(spec.Services))
	for id := range spec.Services {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p.Services = append(p.Services, servicePlan(p, id, spec.Services[id]))
	}

	for i, st := range spec.Steps {
		sp, err := stepPlan(p, i, st)
		if err != nil {
			return nil, err
		}
		p.Steps = append(p.Steps, sp)
	}
	return p, nil
}

// jobNames derives deterministic docker object names for one attempt.
func jobNames(job *threads.FetchResult) Names {
	sum := sha256.Sum256([]byte(job.Repo + "\x00" + job.RunID + "\x00" + job.JobID + "\x00" + fmt.Sprint(job.Attempt)))
	h := hex.EncodeToString(sum[:6])
	return Names{
		Container: "sp-j-" + h,
		Network:   "sp-n-" + h,
		Volume:    "sp-v-" + h,
		Services:  map[string]string{},
	}
}

// builtinEnv is the runner-provided environment (CI conventions: both the
// spindle names and the GitHub-compatible aliases workflows expect).
func builtinEnv(job *threads.FetchResult, ws, serverURL string, matrix map[string]string) map[string]string {
	ref := ""
	if branch := strings.TrimSpace(job.Branch); branch != "" {
		ref = "refs/heads/" + branch
	}
	trusted := "false"
	if job.Trusted {
		trusted = "true"
	}
	env := map[string]string{
		"CI":                "true",
		"GITHUB_ACTIONS":    "true",
		"GITHUB_REPOSITORY": job.Repo,
		"GITHUB_SHA":        job.Commit,
		"GITHUB_REF":        ref,
		"GITHUB_REF_NAME":   job.Branch,
		"GITHUB_EVENT_NAME": job.Event,
		"GITHUB_WORKFLOW":   job.Workflow,
		"GITHUB_JOB":        job.JobID,
		"GITHUB_RUN_ID":     job.RunID,
		"GITHUB_WORKSPACE":  ws,
		"SPINDLE":           "true",
		"SPINDLE_REPO":      job.Repo,
		"SPINDLE_RUN_ID":    job.RunID,
		"SPINDLE_JOB_ID":    job.JobID,
		"SPINDLE_CHECK":     job.Context,
		"SPINDLE_REF":       ref,
		"SPINDLE_SHA":       job.Commit,
		"SPINDLE_EVENT":     job.Event,
		"SPINDLE_WORKFLOW":  job.Workflow,
		"SPINDLE_TRUSTED":   trusted,
		"SPINDLE_WORKSPACE": ws,
		"SPINDLE_ATTEMPT":   fmt.Sprint(job.Attempt),
	}
	if serverURL != "" {
		env["SPINDLE_SERVER"] = strings.TrimRight(serverURL, "/")
	}
	axes := make([]string, 0, len(matrix))
	for axis := range matrix {
		axes = append(axes, axis)
	}
	sort.Strings(axes)
	for _, axis := range axes {
		env["MATRIX_"+envName(axis)] = matrix[axis]
	}
	return env
}

// envName uppercases an axis for the MATRIX_* env spelling.
func envName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// headerValue keeps env values single-line (a CR/LF would corrupt docker's
// option parsing).
func headerValue(v string) string {
	return strings.NewReplacer("\r", "", "\n", " ").Replace(v)
}

// containerArgv builds `docker create` for the job container.
func containerArgv(p *Plan, spec *threads.Spec) []string {
	argv := []string{
		"create", "--name", p.Names.Container,
		"--network", p.Names.Network,
		"--network-alias", "job",
		"--add-host", "host.docker.internal:host-gateway",
		"--workdir", p.Workspace,
		"--mount", "type=volume,source=" + p.Names.Volume + ",target=" + p.Workspace,
	}
	if spec.Container != nil {
		argv = append(argv, splitOptions(spec.Container.Options)...)
		for _, v := range spec.Container.Volumes {
			argv = append(argv, "--volume", v)
		}
	}
	keys := make([]string, 0, len(p.ContainerEnv))
	for k := range p.ContainerEnv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		argv = append(argv, "--env", k+"="+p.ContainerEnv[k])
	}
	// The runner owns the container's lifetime command (GitHub's runner does
	// the same): the image entrypoint is overridden so the container idles
	// across steps.
	argv = append(argv, "--entrypoint", "/bin/sh", p.Image,
		"-c", "trap 'exit 0' TERM INT; while :; do sleep 3600; done")
	return argv
}

// servicePlan builds one sidecar's create argv plus its readiness probes.
func servicePlan(p *Plan, id string, svc threads.ServiceSpec) ServicePlan {
	name := "sp-s-" + p.Names.Container[len("sp-j-"):] + "-" + slug(id)
	p.Names.Services[id] = name
	argv := []string{
		"create", "--name", name,
		"--network", p.Names.Network,
		"--network-alias", id,
	}
	if ep := svc.Entrypoint; len(ep) > 0 {
		argv = append(argv, "--entrypoint", ep[0])
	}
	for _, v := range svc.Volumes {
		argv = append(argv, "--volume", v)
	}
	ports := make([]string, 0, len(svc.Ports))
	probe := make([]string, 0, len(svc.Ports))
	for _, spec := range svc.Ports {
		port := containerPort(spec)
		if port == "" {
			continue
		}
		if !strings.HasSuffix(port, "/udp") {
			// Ephemeral loopback publish: the readiness probe dials it, and
			// concurrent jobs never collide on a fixed host port.
			argv = append(argv, "--publish", "127.0.0.1::"+strings.TrimSuffix(port, "/tcp"))
			probe = append(probe, strings.TrimSuffix(port, "/tcp"))
		}
		ports = append(ports, port)
	}
	keys := make([]string, 0, len(svc.Env))
	for k := range svc.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		argv = append(argv, "--env", k+"="+headerValue(svc.Env[k]))
	}
	if spec := splitOptions(svc.Options); len(spec) > 0 {
		argv = append(argv, spec...)
	}
	argv = append(argv, svc.Image)
	cmd := append([]string{}, svc.Command...)
	if len(svc.Entrypoint) > 1 {
		cmd = append(append([]string{}, svc.Entrypoint[1:]...), cmd...)
	}
	argv = append(argv, cmd...)
	return ServicePlan{ID: id, Name: name, Argv: argv, ProbePorts: probe}
}

// containerPort extracts the container side of a docker port spec
// ("6379", "6379:6379", "127.0.0.1:6379:6379", "6379/udp").
func containerPort(spec string) string {
	parts := strings.Split(strings.TrimSpace(spec), ":")
	return parts[len(parts)-1]
}

// stepPlan resolves one workflow step into a `docker exec` argv.
func stepPlan(p *Plan, index int, st threads.StepSpec) (StepPlan, error) {
	name := subst(p, st.Name)
	if strings.TrimSpace(name) == "" {
		name = fmt.Sprintf("Step %d", index+1)
	}
	sp := StepPlan{
		Index:           index,
		Name:            name,
		If:              subst(p, st.If),
		ContinueOnError: st.ContinueOnError,
		Uses:            st.Uses,
		WorkingDirectory: containerDir(p.Workspace,
			subst(p, st.WorkingDirectory)),
	}
	if st.TimeoutMinutes > 0 {
		sp.Timeout = time.Duration(st.TimeoutMinutes) * time.Minute
	}
	env := map[string]string{}
	for k, v := range p.Env {
		env[k] = v
	}
	for k, v := range st.Env {
		env[k] = subst(p, headerValue(v))
	}
	// The job token is delivered to EVERY step by NAME (15 §5, round-2 pass 2):
	// a `permissions: contents: write` job may push from a script step exactly
	// like GitHub's token, and the value travels in the docker client's
	// environment, never in argv or .git/config. A read-only job simply fails
	// its push at the server (the token carries no push scope).
	if p.JobToken != "" {
		env["SPINDLE_JOB_TOKEN"] = ""
		sp.EnvFrom = append(sp.EnvFrom, "SPINDLE_JOB_TOKEN")
	}
	shell := st.Shell
	switch {
	case strings.TrimSpace(st.Uses) != "":
		planUses(p, &sp, st, env)
	default:
		sp.Kind = "run"
		sp.Run = subst(p, st.Run)
		cmd, cerr := shellCommand(shell, sp.Run)
		if cerr != nil {
			sp.Kind = "unsupported"
			sp.SkipReason = cerr.Error()
			break
		}
		sp.Argv = execArgv(p, sp.WorkingDirectory, env, sp.EnvFrom, cmd)
	}
	return sp, nil
}

// subst applies the spec's expression substitutions to one string: matrix
// cells first, then workflow_dispatch inputs (15 §1/§4.1).
func subst(p *Plan, s string) string {
	return threads.SubstituteInputs(threads.SubstituteMatrix(s, p.Matrix), p.Inputs)
}

// planUses resolves the built-in action set (v1: actions/checkout only). An
// unknown action is recorded as an unsupported step: it fails with a clear
// message instead of being silently ignored.
func planUses(p *Plan, sp *StepPlan, st threads.StepSpec, env map[string]string) {
	action := strings.ToLower(strings.TrimSpace(st.Uses))
	base := action
	if at := strings.IndexByte(base, '@'); at >= 0 {
		base = base[:at]
	}
	switch base {
	case "actions/checkout":
		sp.Kind = "checkout"
		depth := "1"
		if v, ok := st.With["fetch-depth"]; ok && strings.TrimSpace(v) == "0" {
			depth = ""
		}
		sp.Run = checkoutScript(p, depth)
		env["SPINDLE_JOB_TOKEN"] = "" // value supplied via --env NAME
		sp.EnvFrom = append(sp.EnvFrom, "SPINDLE_JOB_TOKEN")
		sp.Argv = execArgv(p, p.Workspace, env, sp.EnvFrom, []string{"sh", "-e", "-c", sp.Run})
	default:
		sp.Kind = "unsupported"
		sp.SkipReason = fmt.Sprintf("unsupported action %q: spindle v1 runs the built-in set only (actions/checkout); there is no marketplace", st.Uses)
	}
}

// checkoutScript builds the in-container clone (15 §4): git, the job token as
// an askpass-style credential helper (never written into .git/config), the
// exact run commit.
func checkoutScript(p *Plan, depth string) string {
	if p.CheckoutURL == "" {
		return "echo 'spindle: no checkout URL in the job payload' >&2; exit 1"
	}
	depthFlag := ""
	if depth != "" {
		depthFlag = "--depth=" + depth + " "
	}
	// The helper's $SPINDLE_JOB_TOKEN is expanded by git's own shell, not ours:
	// it stays inside single quotes here.
	helper := `!f() { echo username=ci; echo password=$SPINDLE_JOB_TOKEN; }; f`
	return strings.Join([]string{
		"set -e",
		"cd " + shQuote(p.Workspace),
		"git init -q",
		"git remote remove origin >/dev/null 2>&1 || true",
		"git remote add origin " + shQuote(p.CheckoutURL),
		"git -c credential.helper=" + shQuote(helper) + " fetch -q " + depthFlag + "origin " + shQuote(p.CheckoutSHA),
		"git checkout -q FETCH_HEAD",
		"echo \"checked out " + p.CheckoutSHA + "\"",
	}, "\n")
}

// execArgv builds `docker exec` with the working dir and the step environment.
// Secret values are appended as `--env NAME` (the value is supplied through the
// docker client's own environment, so it never appears in argv) and masked in
// the log stream.
func execArgv(p *Plan, workdir string, env map[string]string, from []string, cmd []string) []string {
	argv := []string{"exec", "--workdir", workdir}
	keys := make([]string, 0, len(env))
	for k := range env {
		if k == "SPINDLE_JOB_TOKEN" {
			continue // supplied by name below
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		argv = append(argv, "--env", k+"="+env[k])
	}
	secretNames := make([]string, 0, len(p.Secrets))
	for name := range p.Secrets {
		secretNames = append(secretNames, name)
	}
	sort.Strings(secretNames)
	for _, name := range secretNames {
		argv = append(argv, "--env", name)
	}
	// Bare `--env NAME`: the value comes from the docker CLI's own environment
	// (the executor sets it from the plan's token), so it never appears in argv
	// of the exec call, the container's create argv or .git/config.
	for _, name := range from {
		argv = append(argv, "--env", name)
	}
	argv = append(argv, p.Names.Container)
	argv = append(argv, cmd...)
	return argv
}

// shellCommand maps a workflow `shell:` onto an argv prefix.
func shellCommand(shell, script string) ([]string, error) {
	name := strings.TrimSpace(shell)
	if i := strings.IndexByte(name, ' '); i >= 0 {
		name = name[:i] // "bash {0}" and friends: the interpreter is enough
	}
	switch strings.ToLower(name) {
	case "", "sh":
		return []string{"sh", "-e", "-c", script}, nil
	case "bash":
		return []string{"bash", "-eo", "pipefail", "-c", script}, nil
	case "python", "python3":
		return []string{"python3", "-c", script}, nil
	case "pwsh", "powershell":
		return []string{"pwsh", "-command", script}, nil
	case "cmd":
		return []string{"cmd", "/c", script}, nil
	default:
		return nil, fmt.Errorf("unsupported shell %q (v1: sh, bash, python, pwsh, cmd)", shell)
	}
}

// containerDir resolves a workflow working-directory inside the container: an
// absolute path wins, a relative one is joined onto the workspace.
func containerDir(workspace, dir string) string {
	dir = strings.TrimSpace(dir)
	switch {
	case dir == "" || dir == ".":
		return workspace
	case strings.HasPrefix(dir, "/"):
		return strings.TrimRight(dir, "/")
	default:
		dir = strings.TrimPrefix(dir, "./")
		return strings.TrimRight(workspace, "/") + "/" + strings.TrimRight(dir, "/")
	}
}

// rewriteForContainer maps a loopback checkout URL to the container gateway
// (the job container cannot reach the host's 127.0.0.1; every container joins a
// network with host.docker.internal:host-gateway).
func rewriteForContainer(raw string) string {
	for _, host := range []string{"127.0.0.1", "localhost", "[::1]"} {
		if strings.HasPrefix(raw, "http://"+host+":") || strings.HasPrefix(raw, "https://"+host+":") ||
			strings.HasSuffix(raw, "://"+host) {
			return strings.Replace(raw, "://"+host, "://host.docker.internal", 1)
		}
	}
	return raw
}

// splitOptions splits a container/service `options:` string into argv words
// (single/double quotes group, whitespace separates).
func splitOptions(s string) []string {
	var out []string
	var cur strings.Builder
	quote := byte(0)
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote = c
			inWord = true
		case c == ' ' || c == '\t' || c == '\n':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out
}

// slug maps a service id onto the docker name charset.
func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "svc"
	}
	return out
}

// shQuote single-quotes one shell word.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
