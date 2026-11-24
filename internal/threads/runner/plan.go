package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	neturl "net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/threads"
	"github.com/crueber/spindle/internal/threads/workflow"
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

// The docker create --pull policies (docs/threads.md §8.6). "missing" is the
// stock docker behaviour (today's semantics), "always" re-resolves a tag on
// every job, "never" runs only images already on the daemon.
const (
	PullMissing = "missing"
	PullAlways  = "always"
	PullNever   = "never"
)

// ImageConfig is the runner's image-source policy (docs/threads.md §8.1):
// the optional registry prefix every workflow-named image is rewritten under,
// the docker --pull policy, and the per-runner digest requirement. The zero
// value is today's behaviour (no rewrite, --pull=missing, no digest rule).
type ImageConfig struct {
	// Prefix is host[:port]/path of a registry mirror; "" = off.
	Prefix string
	// PrefixAll also rewrites a reference that already names a registry host
	// (its host is dropped and the mirror repository named by Prefix wins).
	PrefixAll bool
	// Pull is missing|always|never; "" means missing.
	Pull string
	// RequireDigest refuses an image without @sha256:<64 hex> at plan time.
	RequireDigest bool
}

// pull returns the effective docker --pull value.
func (c ImageConfig) pull() string {
	if c.Pull == "" {
		return PullMissing
	}
	return c.Pull
}

// imagePrefixRe is a mirror prefix: a registry host (hostname, IP, optional
// port) followed by at least one lowercase path segment. No scheme, no
// trailing slash (docs/threads.md §8.6); the same spelling the server's
// threads.image_allow_prefixes validator accepts.
var imagePrefixRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*(?::[0-9]{1,5})?(?:/[a-z0-9][a-z0-9._-]*)+$`)

// ValidateImageConfig validates the runner's image flags at startup. Every
// error names the flag (and its environment variable) because the operator
// reads startup output, not a manual.
func ValidateImageConfig(c ImageConfig) error {
	prefix := c.Prefix
	switch {
	case prefix == "":
		// Off; --image-prefix-all alone is still a configuration error below.
	case strings.TrimSpace(prefix) != prefix:
		return fmt.Errorf("--image-prefix (SPINDLE_RUNNER_IMAGE_PREFIX): %q has surrounding whitespace", prefix)
	case strings.Contains(prefix, "://"):
		return fmt.Errorf("--image-prefix (SPINDLE_RUNNER_IMAGE_PREFIX): %q must not carry a scheme: give host[:port]/path, e.g. ak.example/dockerhub", prefix)
	case strings.HasSuffix(prefix, "/"):
		return fmt.Errorf("--image-prefix (SPINDLE_RUNNER_IMAGE_PREFIX): %q must not end with '/': give host[:port]/path, e.g. ak.example/dockerhub", prefix)
	case !imagePrefixRe.MatchString(prefix):
		return fmt.Errorf("--image-prefix (SPINDLE_RUNNER_IMAGE_PREFIX): %q must look like host[:port]/path in lowercase, e.g. ak.example/dockerhub or localhost:5000/ci-images", prefix)
	}
	if c.PrefixAll && prefix == "" {
		return fmt.Errorf("--image-prefix-all (SPINDLE_RUNNER_IMAGE_PREFIX_ALL) requires --image-prefix: with no prefix there is nothing to rewrite a registry-named image to")
	}
	switch c.pull() {
	case PullMissing, PullAlways, PullNever:
	default:
		return fmt.Errorf("--image-pull (SPINDLE_RUNNER_IMAGE_PULL): %q is not one of %s, %s, %s (docker create --pull)", c.Pull, PullMissing, PullAlways, PullNever)
	}
	return nil
}

// splitImageRef separates a reference's name from its tag or digest
// ("golang:1.27" → "golang", ":1.27"; "a/b@sha256:…" → "a/b", "@sha256:…").
// An IPv6 host's brackets keep its colons out of the tag search because the
// tag must follow the last '/'.
func splitImageRef(ref string) (name, suffix string) {
	if i := strings.LastIndexByte(ref, '@'); i >= 0 {
		return ref[:i], ref[i:]
	}
	if i := strings.LastIndexByte(ref, ':'); i > strings.LastIndexByte(ref, '/') {
		return ref[:i], ref[i:]
	}
	return ref, ""
}

// refRegistry returns a reference's leading registry host, or "" when the
// reference is host-less (docker's own rule: the first segment is a registry
// when it contains '.' or ':', or is 'localhost').
func refRegistry(name string) string {
	first := name
	if i := strings.IndexByte(name, '/'); i >= 0 {
		first = name[:i]
	}
	if strings.ContainsAny(first, ".:") || strings.EqualFold(first, "localhost") {
		return first
	}
	return ""
}

// RewriteImage applies the runner's --image-prefix rule to one reference
// (docs/threads.md §8.6):
//
//   - no prefix, or an empty reference: unchanged;
//   - a host-less reference (golang:1.27, alpine/git:latest) is a Docker Hub
//     reference and becomes <prefix>/<path>[:tag|@digest];
//   - a reference that names a registry (ghcr.io/a/b:t, localhost:5000/a/b)
//     is unchanged unless ImageConfig.PrefixAll is set, in which case the
//     registry host is replaced by the prefix: the host is DROPPED, not
//     carried, so a mirror with one repository per upstream is named by the
//     prefix itself (<prefix> for ghcr.io, a different prefix for Docker Hub).
//
// Docker Hub's implicit "library/" namespace is deliberately NOT inserted or
// stripped: the reference travels exactly as written and the upstream (the
// mirror's proxy repository, or docker itself) applies its own normalisation.
// "golang:1.27" therefore becomes <prefix>/golang:1.27, which a Docker Hub
// proxy resolves to library/golang, while "docker.io/library/golang:1.27"
// names a registry and only moves under PrefixAll (to <prefix>/library/golang).
func RewriteImage(ref string, c ImageConfig) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || c.Prefix == "" {
		return ref
	}
	name, suffix := splitImageRef(ref)
	registry := refRegistry(name)
	if registry == "" {
		return strings.TrimRight(c.Prefix, "/") + "/" + name + suffix
	}
	if !c.PrefixAll {
		return ref
	}
	path := strings.TrimPrefix(name, registry+"/")
	if path == name || path == "" {
		return ref // not a registry/path shape after all: never mangle it
	}
	return strings.TrimRight(c.Prefix, "/") + "/" + path + suffix
}

// HasImageDigest reports whether a reference is pinned to a sha256 digest
// (@sha256:<64 lowercase hex>), docker's own grammar for a digest.
func HasImageDigest(ref string) bool {
	name, suffix := splitImageRef(ref)
	if name == ref || !strings.HasPrefix(suffix, "@sha256:") {
		return false
	}
	digest := strings.TrimPrefix(suffix, "@sha256:")
	if len(digest) != 64 {
		return false
	}
	for _, c := range digest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// requireDigestError is the plan-time refusal for one image
// (--image-require-digest).
func requireDigestError(kind, image string) error {
	return fmt.Errorf("%s image %q is not digest-pinned and this runner requires a digest (--image-require-digest / SPINDLE_RUNNER_IMAGE_REQUIRE_DIGEST): pin it as %s@sha256:<64 hex> (docker image inspect --format '{{index .RepoDigests 0}}' %s)", kind, image, strings.SplitN(image, "@", 2)[0], image)
}

// PlanInput is everything needed to plan one job.
type PlanInput struct {
	Job *threads.FetchResult
	// AttemptToken is the wjt_ credential (checkout password; read-only).
	AttemptToken string
	DefaultImage string
	WorkspaceDir string
	// Images is the runner's image-source policy (--image-prefix/-all,
	// --image-pull, --image-require-digest); the zero value changes nothing.
	Images ImageConfig
	// ServerURL is the spindle origin the runner was configured with (exposed
	// as SPINDLE_SERVER; informational).
	ServerURL string
	// Caps is the server's artifact cap advertisement (ExecInput.Caps).
	Caps threads.ArtifactCaps
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
	JobRef  string
	Attempt int
	Names   Names
	// Workspace is the in-container working directory.
	Workspace string
	// Image is the reference actually created (after the image-prefix
	// rewrite); RequestedImage is what the workflow (or --image) named, and
	// differs only when the rewrite changed it.
	Image          string
	RequestedImage string
	// Images is the runner's image-source policy this plan was built with
	// (the create-failure diagnostic and --pull= read it).
	Images ImageConfig
	// CheckoutURL is the URL the job container actually fetches from (a
	// loopback origin is rewritten to host.docker.internal).
	CheckoutURL string
	// CheckoutOriginURL is the URL the server put in the payload, before that
	// rewrite — reported alongside CheckoutURL so an operator sees both ends.
	CheckoutOriginURL string
	CheckoutSHA       string
	Timeout           time.Duration

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
	// IDTokenRequestToken is the job's OIDC request token, delivered to every
	// repository-code step by NAME (never argv; value from the docker client's
	// environment). Empty when the job did not declare id-token: write.
	IDTokenRequestToken string
	// Matrix/Inputs feed `if`, substitution and the environment; kept for
	// reporting.
	Matrix map[string]string
	Inputs map[string]string
	Env    map[string]string
	// Needs is the folded output map of the job's `needs:` dependencies (the
	// server materializes it at claim time). Step fields read it as
	// `${{ needs.<job>.outputs.<name> }}`.
	Needs map[string]map[string]string
	// OutputTemplates are the job's declared `outputs:` templates (matrix and
	// inputs already substituted), evaluated against the collected step
	// outputs when the job finishes (docs/threads.md §3.9).
	OutputTemplates map[string]string
	// ArtifactCaps are the server's upload caps (zero = the service defaults).
	ArtifactCaps threads.ArtifactCaps
	// Actions are the repository-local action definitions embedded in the job
	// payload at creation (docs/threads.md §3.4a). They are pinned to the run
	// commit; the runner never reads a repository, so an expansion can never
	// pick up a file from another ref.
	Actions map[string]threads.ActionSpec
}

// ServicePlan is one sidecar.
type ServicePlan struct {
	ID   string
	Name string
	Argv []string // docker create … minus "docker"
	// Image is the reference actually created (after the prefix rewrite);
	// RequestedImage is what the workflow named (the diagnostic states both).
	Image          string
	RequestedImage string
	// ProbePorts are the container TCP ports the runner polls for readiness
	// (published on loopback ephemeral ports).
	ProbePorts []string
}

// StepPlan is one step's exec invocation.
type StepPlan struct {
	Index int
	// OutputIndex is the step's position in the FLAT execution order: a
	// composite action's inner steps each need their own output file even
	// though they share the caller's display index.
	OutputIndex int
	// ID is the step's `id:` ("" for an anonymous step); the executor records
	// the step's outcome/conclusion under it for a later step's `if:`, and
	// collects the outputs it publishes through $SPINDLE_OUTPUT. A step inside
	// a composite action has no job-level id: LocalID is its action-local one.
	ID               string
	Name             string
	Kind             string // run | checkout | docker | composite | artifact-* | unsupported
	Uses             string
	If               string
	ContinueOnError  bool
	Timeout          time.Duration
	WorkingDirectory string
	Run              string
	// Parent is the display name chain of the composite action this step was
	// expanded from ("" for a top-level step).
	Parent string
	// LocalID is a step's id WITHIN a composite action. Its outputs are
	// visible to the action's other steps and its outputs: mapping, never to
	// the job's steps.<id> context.
	LocalID string
	// Composite is set on a step that expands into a local composite action:
	// the inner steps run in order and the action's declared outputs are
	// evaluated afterwards.
	Composite *CompositePlan
	// Docker is set on a docker:// step (and a local docker action): the image
	// actually run (after the runner's prefix rewrite) and the metadata's
	// entrypoint/args/env.
	Docker *DockerPlan
	// OutputPath is the in-container file this step may write outputs to
	// ($SPINDLE_OUTPUT, aliased as $GITHUB_OUTPUT). Only a step with an id is
	// collected (docs/threads.md §3.9).
	OutputPath string
	// Argv is `docker exec …` minus the leading "docker" (or `docker run …`
	// for a docker action).
	Argv []string
	// EnvFrom are variable names supplied via `--env NAME` (values come from
	// the docker client's environment).
	EnvFrom []string
	// SkipReason is set for an unsupported action (the step fails with it).
	SkipReason string
	// Artifact is set for the two built-in artifact actions (15 §2.5):
	// upload-artifact / download-artifact.
	Artifact *ArtifactStep
	// Template holds the step's fields after the plan-time matrix/inputs
	// substitution and before the run-time `steps.<id>.outputs.*`
	// substitution. The executor re-resolves the step against the outputs
	// collected so far before it runs (ResolveStep).
	Template *StepTemplate
}

// CompositePlan is one expanded local composite action.
type CompositePlan struct {
	// Path is the repository-relative action directory.
	Path string
	// Steps are the action's steps, expanded recursively in order.
	Steps []StepPlan
	// Outputs maps the action's declared output names to the templates
	// evaluated over its own steps' outputs.
	Outputs map[string]string
}

// DockerPlan is one docker action invocation's metadata.
type DockerPlan struct {
	// Path is the local action directory ("" for an inline docker:// step).
	Path string
	// Image is the reference actually run (after --image-prefix);
	// RequestedImage is what the workflow or metadata named.
	Image          string
	RequestedImage string
	// Entrypoint/Args/Env come from the action metadata (docker actions only)
	// and are empty for an inline docker:// step: the image's own entrypoint
	// and command are used then.
	Entrypoint []string
	Args       []string
	Env        map[string]string
}

// StepTemplate is a step's unresolved fields (matrix/inputs already applied).
type StepTemplate struct {
	Name             string
	Run              string
	If               string
	Shell            string
	Uses             string
	WorkingDirectory string
	// With/Env keep their authored values; matrix/inputs were substituted
	// into the string fields, and the run-time step outputs are substituted at
	// execution.
	With map[string]string
	Env  map[string]string
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
	requestedImage := image
	if spec.Container != nil && strings.TrimSpace(spec.Container.Image) != "" {
		image = strings.TrimSpace(spec.Container.Image)
		requestedImage = image
	}
	if err := ValidateImageConfig(in.Images); err != nil {
		return nil, err
	}
	// The image-prefix rewrite applies to the runner's own default image too:
	// a mirror covers "the workflow named nothing" exactly like it covers a
	// workflow-named image (docs/threads.md §8.6).
	image = RewriteImage(requestedImage, in.Images)
	if in.Images.RequireDigest && !HasImageDigest(image) {
		return nil, requireDigestError("job", image)
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

	idTokenRequestToken := ""
	if job.IDToken != nil {
		idTokenRequestToken = job.IDToken.RequestToken
		// The request token is a bearer credential: mask it in this runner's
		// log stream exactly like a repository secret (best effort, 15 §5.1).
		if idTokenRequestToken != "" {
			mask = append(mask, idTokenRequestToken)
			sort.Strings(mask)
		}
	}

	p := &Plan{
		JobRef: job.JobRef, Attempt: job.Attempt,
		Names: names, Workspace: ws, Image: image, RequestedImage: requestedImage,
		Images:      in.Images,
		CheckoutSHA: job.Commit, Secrets: secretEnv, Mask: mask,
		JobToken:            in.AttemptToken,
		IDTokenRequestToken: idTokenRequestToken,
		Matrix:              matrix, Inputs: inputs, Env: jobEnv,
		Needs:        job.NeedsOutputs,
		ArtifactCaps: in.Caps,
		Actions:      spec.Actions,
	}
	// The job's output templates are plain strings here (matrix/inputs
	// resolved); each reference to a step output is resolved from the collected
	// outputs when the job finishes.
	if len(spec.Outputs) > 0 {
		p.OutputTemplates = make(map[string]string, len(spec.Outputs))
		for name, tmpl := range spec.Outputs {
			p.OutputTemplates[name] = subst(p, tmpl)
		}
	}
	if job.Checkout != nil {
		p.CheckoutOriginURL = job.Checkout.URL
		p.CheckoutURL = rewriteForContainer(job.Checkout.URL)
	}
	if spec.TimeoutMinutes > 0 {
		p.Timeout = time.Duration(spec.TimeoutMinutes) * time.Minute
	}
	p.ContainerEnv = jobEnv
	argv, cerr := containerArgv(p, spec)
	if cerr != nil {
		return nil, cerr
	}
	p.ContainerArgv = argv

	ids := make([]string, 0, len(spec.Services))
	for id := range spec.Services {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		svc, serr := servicePlan(p, id, spec.Services[id])
		if serr != nil {
			return nil, serr
		}
		p.Services = append(p.Services, svc)
	}

	next := 0
	for i, st := range spec.Steps {
		planned, err := planStep(p, i, st, expandCtx{depth: 1}, &next)
		if err != nil {
			return nil, err
		}
		p.Steps = append(p.Steps, planned...)
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
	// The OIDC exchange URL (never the token — that travels by NAME only, so
	// it never reaches docker argv/inspect; docs/threads.md §5.5). Both the
	// spindle spelling and the GitHub-compatible alias are set because
	// workflows already written for GitHub read ACTIONS_ID_TOKEN_REQUEST_URL.
	if job.IDToken != nil && strings.TrimSpace(job.IDToken.RequestURL) != "" {
		env["SPINDLE_ID_TOKEN_REQUEST_URL"] = job.IDToken.RequestURL
		env["ACTIONS_ID_TOKEN_REQUEST_URL"] = job.IDToken.RequestURL
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
func containerArgv(p *Plan, spec *threads.Spec) ([]string, error) {
	argv := []string{
		"create", "--name", p.Names.Container,
		"--network", p.Names.Network,
		"--network-alias", "job",
		"--add-host", "host.docker.internal:host-gateway",
		"--workdir", p.Workspace,
		// The pull policy is always explicit: docker's own default (missing)
		// and the operator's choice are then one observable argv, and a moved
		// tag reaches CI only when the operator asked for --image-pull=always
		// (docs/threads.md §8.6).
		"--pull=" + p.Images.pull(),
		"--mount", "type=volume,source=" + p.Names.Volume + ",target=" + p.Workspace,
	}
	if spec.Container != nil {
		// The workflow's options and volumes pass the secure-by-default
		// allowlist BEFORE a single argv element is produced: a refused job
		// must not start (docs/threads.md §8.7).
		if err := checkWorkflowContainer("container", spec.Container.Options, spec.Container.Volumes); err != nil {
			return nil, err
		}
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
	return argv, nil
}

// servicePlan builds one sidecar's create argv plus its readiness probes. The
// sidecar image goes through the same prefix rewrite, --pull policy and digest
// rule as the job image: a service is workflow-named code too.
func servicePlan(p *Plan, id string, svc threads.ServiceSpec) (ServicePlan, error) {
	name := "sp-s-" + p.Names.Container[len("sp-j-"):] + "-" + slug(id)
	p.Names.Services[id] = name
	requested := svc.Image
	image := RewriteImage(requested, p.Images)
	if p.Images.RequireDigest && !HasImageDigest(image) {
		return ServicePlan{}, requireDigestError(fmt.Sprintf("service %q", id), image)
	}
	// A sidecar's options and mounts are workflow-controlled exactly like the
	// job container's, and they land on the same daemon (docs/threads.md §8.7).
	if err := checkWorkflowContainer("services."+id, svc.Options, svc.Volumes); err != nil {
		return ServicePlan{}, err
	}
	argv := []string{
		"create", "--name", name,
		"--network", p.Names.Network,
		"--network-alias", id,
		"--pull=" + p.Images.pull(),
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
	argv = append(argv, image)
	cmd := append([]string{}, svc.Command...)
	if len(svc.Entrypoint) > 1 {
		cmd = append(append([]string{}, svc.Entrypoint[1:]...), cmd...)
	}
	argv = append(argv, cmd...)
	return ServicePlan{ID: id, Name: name, Argv: argv, Image: image, RequestedImage: requested, ProbePorts: probe}, nil
}

// containerPort extracts the container side of a docker port spec
// ("6379", "6379:6379", "127.0.0.1:6379:6379", "6379/udp").
func containerPort(spec string) string {
	parts := strings.Split(strings.TrimSpace(spec), ":")
	return parts[len(parts)-1]
}

// expandCtx carries one step list's expansion state: the composite nesting
// level (workflow steps are 1), the open action directories (cycle detection),
// the display parent chain and the environment/working directory inherited from
// the calling step.
type expandCtx struct {
	depth   int
	stack   []string
	parent  string
	env     map[string]string
	workdir string
}

// planStep resolves one authored step into one or more plans. A plain step is
// one plan; a local composite action becomes one composite plan holding its
// expanded inner steps (recursively), which the executor runs in order. next is
// the flat execution index (unique output files for inner steps).
func planStep(p *Plan, index int, st threads.StepSpec, ctx expandCtx, next *int) ([]StepPlan, error) {
	uses := strings.TrimSpace(st.Uses)
	if threads.IsLocalAction(uses) {
		return planLocalAction(p, index, st, ctx, next)
	}
	if image := threads.DockerUsesImage(uses); image != "" {
		return planDockerStep(p, index, st, &DockerPlan{RequestedImage: image}, st.With, ctx, next)
	}
	sp, err := stepPlan(p, index, st, ctx, next)
	if err != nil {
		return nil, err
	}
	return []StepPlan{sp}, nil
}

// planLocalAction expands a `uses: ./<path>` step: a composite action becomes
// a CompositePlan, a docker action a single docker step. Both definitions were
// embedded in the payload at creation; a definition the payload does not carry
// is a named plan error (the job fails before any container starts).
func planLocalAction(p *Plan, index int, st threads.StepSpec, ctx expandCtx, next *int) ([]StepPlan, error) {
	uses := strings.TrimSpace(st.Uses)
	dir, err := threads.LocalActionDir(uses)
	if err != nil {
		return nil, fmt.Errorf("step %d (uses: %s): %v", index+1, uses, err)
	}
	def, ok := p.Actions[dir]
	if !ok {
		return nil, fmt.Errorf("step %d (uses: %s): local action %q is not in the job payload (the run was created without its definition)", index+1, uses, dir)
	}
	switch def.Using {
	case "composite":
		return planCompositeStep(p, index, st, def, ctx, next)
	case "docker":
		with, bad := actionWith(def, st.With)
		if len(bad) > 0 {
			return nil, fmt.Errorf("step %d (uses: %s): input %q is not declared by the action", index+1, uses, bad[0])
		}
		dp := &DockerPlan{
			Path: dir, RequestedImage: def.Image, Entrypoint: def.Entrypoint,
			Args: def.Args, Env: def.Env,
		}
		return planDockerStep(p, index, st, dp, with, ctx, next)
	default:
		return nil, fmt.Errorf("step %d (uses: %s): local action %q has unsupported using %q", index+1, uses, dir, def.Using)
	}
}

// planCompositeStep expands a local composite action in place. The caller's
// `env:` applies to every inner step, `with:` values (and the action's declared
// defaults) become INPUT_* environment variables, and an inner step's working
// directory is relative to the caller's.
func planCompositeStep(p *Plan, index int, st threads.StepSpec, def threads.ActionSpec, ctx expandCtx, next *int) ([]StepPlan, error) {
	uses := strings.TrimSpace(st.Uses)
	dir := def.Path
	if ctx.depth > workflow.MaxActionDepth {
		return nil, fmt.Errorf("step %d (uses: %s): composite actions are nested deeper than the limit of %d", index+1, uses, workflow.MaxActionDepth)
	}
	for i, open := range ctx.stack {
		if open == dir {
			chain := append(append([]string{}, ctx.stack[i:]...), dir)
			return nil, fmt.Errorf("step %d (uses: %s): composite action cycle: %s", index+1, uses, strings.Join(chain, " → "))
		}
	}
	with, bad := actionWith(def, st.With)
	if len(bad) > 0 {
		return nil, fmt.Errorf("step %d (uses: %s): input %q is not declared by the action", index+1, uses, bad[0])
	}
	name := strings.TrimSpace(st.Name)
	if name == "" {
		name = dir
	}
	parent := name
	if ctx.parent != "" {
		parent = ctx.parent + " / " + name
	}
	// The caller's env applies to its steps; INPUT_* wins so an env block
	// cannot silently shadow the action's inputs. An ENCLOSING action's INPUT_*
	// names are not inherited: INPUT_* is the current action's interface, so a
	// nested action sees its own inputs only.
	childEnv := withoutInputs(ctx.env)
	for k, v := range st.Env {
		childEnv[k] = v
	}
	for k, v := range inputEnv(def.Inputs, with) {
		childEnv[k] = v
	}
	base := p.Workspace
	if ctx.workdir != "" {
		base = ctx.workdir
	}
	if wd := strings.TrimSpace(st.WorkingDirectory); wd != "" {
		base = containerDir(base, wd)
	}
	caller := StepPlan{
		Index: index, Parent: ctx.parent, ID: strings.ToLower(strings.TrimSpace(st.ID)),
		Kind: "composite", Uses: st.Uses, ContinueOnError: st.ContinueOnError,
		Composite: &CompositePlan{Path: dir, Outputs: def.Outputs},
		Template: &StepTemplate{
			Name: subst(p, st.Name), If: subst(p, st.If), Uses: st.Uses,
		},
	}
	if st.TimeoutMinutes > 0 {
		caller.Timeout = time.Duration(st.TimeoutMinutes) * time.Minute
	}
	inner := expandCtx{depth: ctx.depth + 1, parent: parent, env: childEnv, workdir: base}
	inner.stack = append(append([]string{}, ctx.stack...), dir)
	for i, child := range def.Steps {
		planned, err := planStep(p, index, child, inner, next)
		if err != nil {
			return nil, fmt.Errorf("step %d (uses: %s) step %d: %w", index+1, uses, i+1, err)
		}
		if len(planned) > 0 {
			planned[0].LocalID = strings.ToLower(strings.TrimSpace(child.ID))
		}
		caller.Composite.Steps = append(caller.Composite.Steps, planned...)
	}
	resolved, err := resolveStep(p, caller, func(s string) (string, error) { return s, nil })
	if err != nil {
		return nil, fmt.Errorf("step %d (uses: %s): %w", index+1, uses, err)
	}
	return []StepPlan{resolved}, nil
}

// planDockerStep plans one docker action invocation: a docker:// step, or a
// local docker action (metadata supplies entrypoint/args/env and defaults).
// The image passes the runner's own image policy here — BuildPlan runs before
// the network, volume or any container exists, so a refusal fails the job with
// nothing started.
func planDockerStep(p *Plan, index int, st threads.StepSpec, dp *DockerPlan, with map[string]string, ctx expandCtx, next *int) ([]StepPlan, error) {
	image := RewriteImage(strings.TrimSpace(dp.RequestedImage), p.Images)
	if image == "" {
		return nil, fmt.Errorf("step %d: docker action %q has no image", index+1, st.Uses)
	}
	if p.Images.RequireDigest && !HasImageDigest(image) {
		return nil, fmt.Errorf("step %d (uses: %s): %s", index+1, st.Uses, requireDigestError("action", image))
	}
	dp.Image = image
	env := withoutInputs(ctx.env)
	for k, v := range dp.Env {
		env[k] = headerValue(v)
	}
	for k, v := range st.Env {
		env[k] = v
	}
	wd := ctx.workdir
	if wd == "" {
		wd = p.Workspace
	}
	sp := StepPlan{
		Index: index, Parent: ctx.parent, Kind: "docker", Uses: st.Uses,
		ID:              strings.ToLower(strings.TrimSpace(st.ID)),
		ContinueOnError: st.ContinueOnError, Docker: dp,
		Template: &StepTemplate{
			Name:             subst(p, st.Name),
			If:               subst(p, st.If),
			Uses:             st.Uses,
			WorkingDirectory: containerDir(wd, st.WorkingDirectory),
			With:             copyStrings(with),
			Env:              copyStrings(env),
		},
	}
	if st.TimeoutMinutes > 0 {
		sp.Timeout = time.Duration(st.TimeoutMinutes) * time.Minute
	}
	sp.OutputIndex = *next
	*next++
	sp.OutputPath = outputFilePath(sp.OutputIndex)
	resolved, err := resolveStep(p, sp, func(s string) (string, error) { return s, nil })
	if err != nil {
		return nil, err
	}
	return []StepPlan{resolved}, nil
}

// withoutInputs copies env without an enclosing action's INPUT_* entries.
func withoutInputs(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for k, v := range env {
		if strings.HasPrefix(k, "INPUT_") {
			continue
		}
		out[k] = v
	}
	return out
}

// actionWith fills an action's declared input defaults and overlays the
// caller's `with:`. Unknown caller keys are returned (sorted) for a refusal:
// an input name that does nothing is a typo, never silently ignored.
func actionWith(def threads.ActionSpec, with map[string]string) (map[string]string, []string) {
	out := make(map[string]string, len(def.Inputs)+len(with))
	for name, in := range def.Inputs {
		out[name] = in.Default
	}
	var bad []string
	for name, val := range with {
		if _, ok := def.Inputs[name]; !ok {
			bad = append(bad, name)
			continue
		}
		out[name] = val
	}
	sort.Strings(bad)
	return out, bad
}

// inputEnv maps filled input values onto the INPUT_* environment (uppercase,
// spaces → underscores: GitHub's rule). Every declared input is present (empty
// when it has neither a default nor a supplied value), so a script can test
// INPUT_X without an unset-variable guess.
func inputEnv(inputs map[string]threads.ActionInputSpec, with map[string]string) map[string]string {
	env := make(map[string]string, len(inputs)+len(with))
	for name := range inputs {
		env[workflow.InputEnvName(name)] = with[name]
	}
	for name, val := range with {
		env[workflow.InputEnvName(name)] = val
	}
	return env
}

// stepPlan resolves one workflow step into a `docker exec` argv. The fields are
// kept as a StepTemplate too, because a step may read an earlier step's outputs
// — a value only known at execution time (ResolveStep re-resolves and rebuilds
// the argv just before the step runs).
func stepPlan(p *Plan, index int, st threads.StepSpec, ctx expandCtx, next *int) (StepPlan, error) {
	env := map[string]string{}
	for k, v := range ctx.env {
		env[k] = v
	}
	for k, v := range st.Env {
		env[k] = v
	}
	wd := st.WorkingDirectory
	if ctx.workdir != "" {
		wd = containerDir(ctx.workdir, st.WorkingDirectory)
	}
	tmpl := &StepTemplate{
		Name:             subst(p, st.Name),
		Run:              subst(p, st.Run),
		If:               subst(p, st.If),
		Shell:            st.Shell,
		Uses:             st.Uses,
		WorkingDirectory: wd,
		With:             copyStrings(st.With),
		Env:              copyStrings(env),
	}
	sp := StepPlan{
		Index:           index,
		Parent:          ctx.parent,
		ID:              strings.ToLower(strings.TrimSpace(st.ID)),
		ContinueOnError: st.ContinueOnError,
		Template:        tmpl,
	}
	sp.OutputIndex = *next
	*next++
	sp.OutputPath = outputFilePath(sp.OutputIndex)
	if st.TimeoutMinutes > 0 {
		sp.Timeout = time.Duration(st.TimeoutMinutes) * time.Minute
	}
	return resolveStep(p, sp, func(s string) (string, error) { return s, nil })
}

// ResolveStep re-resolves a planned step against the outputs collected so far,
// rebuilding its argv. The executor calls it immediately before a step runs; a
// reference that cannot be resolved is a named error and fails the step
// (docs/threads.md §3.9). Step outputs and the job's `needs:` outputs are both
// substituted (a field may read either).
func (p *Plan) ResolveStep(step StepPlan, outputs map[string]map[string]string) (StepPlan, error) {
	return resolveStep(p, step, func(s string) (string, error) {
		return threads.SubstituteOutputs(s, outputs, p.Needs)
	})
}

// ResolveChildStep re-resolves one composite action inner step against the
// job's collected outputs merged with the action's own step outputs. Inside the
// action an action-local id is what `steps.<id>.outputs.<x>` means; a local id
// shadows a job id of the same name (the action is its own namespace), which is
// documented in docs/threads.md §3.4a.
func (p *Plan) ResolveChildStep(step StepPlan, jobOutputs, local map[string]map[string]string) (StepPlan, error) {
	merged := make(map[string]map[string]string, len(jobOutputs)+len(local))
	for id, outs := range jobOutputs {
		merged[id] = outs
	}
	for id, outs := range local {
		merged[id] = outs
	}
	return resolveStep(p, step, func(s string) (string, error) {
		return threads.SubstituteOutputs(s, merged, p.Needs)
	})
}

// resolveStep fills a step's resolved fields from its template and (re)builds
// the docker argv. resolve is the substitution applied to every template field
// (identity for the plan preview, step outputs at execution).
func resolveStep(p *Plan, sp StepPlan, resolve func(string) (string, error)) (StepPlan, error) {
	t := sp.Template
	if t == nil {
		return sp, nil // a hand-built StepPlan (tests, the artifact helpers)
	}
	// Matrix/inputs substitution is idempotent, so applying it here also
	// covers the raw With/Env values (the string fields already carry it).
	expand := func(s string) (string, error) {
		return resolve(subst(p, s))
	}
	name, err := expand(t.Name)
	if err != nil {
		return sp, err
	}
	if strings.TrimSpace(name) == "" {
		name = fmt.Sprintf("Step %d", sp.Index+1)
	}
	sp.Name = name
	if sp.If, err = expand(t.If); err != nil {
		return sp, err
	}
	if sp.Run, err = expand(t.Run); err != nil {
		return sp, err
	}
	wd, err := expand(t.WorkingDirectory)
	if err != nil {
		return sp, err
	}
	sp.Uses = t.Uses
	sp.WorkingDirectory = containerDir(p.Workspace, wd)
	sp.Kind, sp.SkipReason, sp.Argv, sp.Artifact = "", "", nil, nil
	sp.EnvFrom = nil
	env := make(map[string]string, len(p.Env)+len(t.Env)+2)
	for k, v := range p.Env {
		env[k] = v
	}
	for k, v := range t.Env {
		// The authored value is flattened first (an authored newline would be a
		// typo); a substituted OUTPUT value keeps its newlines — it is one argv
		// element to docker, and a multi-line output is legitimate data.
		val, verr := expand(headerValue(v))
		if verr != nil {
			return sp, verr
		}
		env[k] = val
	}
	with := make(map[string]string, len(t.With))
	for k, v := range t.With {
		val, verr := expand(v)
		if verr != nil {
			return sp, verr
		}
		with[k] = val
	}
	// The job token is delivered to EVERY script step by NAME (15 §5, round-2
	// pass 2): a `permissions: contents: write` job may push from a script step
	// exactly like GitHub's token, and the value travels in the docker client's
	// environment, never in argv or .git/config. A read-only job simply fails
	// its push at the server (the token carries no push scope).
	//
	// A docker action is the one step that NEVER receives it: the image is
	// third-party code, and a fork run's contract is "read-only, no secrets".
	// A composite action's `run:` steps are repository code (the same trust as
	// a workflow script), so they do receive it like any other run step.
	isDocker := sp.Docker != nil || threads.DockerUsesImage(t.Uses) != ""
	if p.JobToken != "" && !isDocker && sp.Composite == nil {
		env["SPINDLE_JOB_TOKEN"] = ""
		sp.EnvFrom = append(sp.EnvFrom, "SPINDLE_JOB_TOKEN")
	}
	// The OIDC request token follows the job token's rule exactly: delivered to
	// repository-code steps by NAME (value from the docker client's
	// environment), never to a docker action image (third-party code), and
	// never in argv. The URL is already in the container env (builtinEnv).
	if p.IDTokenRequestToken != "" && !isDocker && sp.Composite == nil {
		env["SPINDLE_ID_TOKEN_REQUEST_TOKEN"] = ""
		sp.EnvFrom = append(sp.EnvFrom, "SPINDLE_ID_TOKEN_REQUEST_TOKEN")
		env["ACTIONS_ID_TOKEN_REQUEST_TOKEN"] = ""
		sp.EnvFrom = append(sp.EnvFrom, "ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	}
	// Every step gets the output file GitHub-style; only a step with an id is
	// collected (an anonymous step's writes are discarded, as on GitHub).
	if sp.OutputPath != "" {
		env[OutputEnvVar] = sp.OutputPath
		env[OutputEnvAlias] = sp.OutputPath
	}
	switch {
	case sp.Composite != nil:
		// Expanded at plan time; the composite itself has no command. The
		// children were planned against this step's inputs.
		sp.Kind = "composite"
	case isDocker:
		sp.Kind = "docker"
		if sp.Docker == nil {
			sp.Docker = &DockerPlan{RequestedImage: threads.DockerUsesImage(t.Uses)}
		}
		// A docker:// step's `with:` values are its inputs (GitHub's rule):
		// every key reaches the container as INPUT_<NAME>.
		for name, val := range with {
			env[workflow.InputEnvName(name)] = val
		}
		sp.Argv = dockerRunArgv(p, sp, env)
	case strings.TrimSpace(t.Uses) != "":
		planUses(p, &sp, t.Uses, with, env)
	default:
		sp.Kind = "run"
		cmd, cerr := shellCommand(t.Shell, sp.Run)
		if cerr != nil {
			sp.Kind = "unsupported"
			sp.SkipReason = cerr.Error()
			break
		}
		sp.Argv = execArgv(p, sp.WorkingDirectory, env, sp.EnvFrom, cmd)
	}
	return sp, nil
}

// dockerActionName is the deterministic container name of one docker action
// step: cleanup can remove it on every path without tracking state, and
// concurrent jobs cannot collide.
func dockerActionName(p *Plan, outputIndex int) string {
	suffix := p.Names.Container
	if strings.HasPrefix(suffix, "sp-j-") {
		suffix = suffix[len("sp-j-"):]
	}
	return "sp-a-" + suffix + "-" + fmt.Sprint(outputIndex+1)
}

// dockerRunArgv builds `docker run` for a docker action. The container joins
// the job's network (identical network policy to the job container), mounts the
// job workspace at the SAME path (same files, same paths), runs in the step's
// working directory and takes the image's own entrypoint and command unless the
// action metadata names one. It never gets the host docker socket, never
// --privileged, and no capabilities are added; secret values are never in env
// (the executor also passes no secret environment to the docker CLI for this
// step), so a docker action cannot read the job token or repository secrets.
func dockerRunArgv(p *Plan, sp StepPlan, env map[string]string) []string {
	argv := []string{
		"run", "--name", dockerActionName(p, sp.OutputIndex),
		"--network", p.Names.Network,
		"--add-host", "host.docker.internal:host-gateway",
		"--workdir", sp.WorkingDirectory,
		"--pull=" + p.Images.pull(),
		"--mount", "type=volume,source=" + p.Names.Volume + ",target=" + p.Workspace,
	}
	if d := sp.Docker; d != nil && len(d.Entrypoint) > 0 {
		argv = append(argv, "--entrypoint", d.Entrypoint[0])
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		// Never: neither the job token nor any OIDC request material is part
		// of a docker action's environment (the image is third-party code).
		if k == "SPINDLE_JOB_TOKEN" || k == "SPINDLE_ID_TOKEN_REQUEST_URL" ||
			k == "ACTIONS_ID_TOKEN_REQUEST_URL" || k == "SPINDLE_ID_TOKEN_REQUEST_TOKEN" ||
			k == "ACTIONS_ID_TOKEN_REQUEST_TOKEN" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		argv = append(argv, "--env", k+"="+env[k])
	}
	argv = append(argv, sp.Docker.Image)
	if d := sp.Docker; d != nil {
		if len(d.Entrypoint) > 1 {
			argv = append(argv, d.Entrypoint[1:]...)
		}
		argv = append(argv, d.Args...)
	}
	return argv
}

// copyStrings clones a map so a resolved step never mutates the job spec.
func copyStrings(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// subst applies the spec's expression substitutions to one string: matrix
// cells first, then workflow_dispatch inputs (15 §1/§4.1).
func subst(p *Plan, s string) string {
	return threads.SubstituteInputs(threads.SubstituteMatrix(s, p.Matrix), p.Inputs)
}

// planUses resolves the built-in action set (15 §2, §2.5): actions/checkout,
// actions/upload-artifact and actions/download-artifact. An unknown action is
// recorded as an unsupported step: it fails with a clear message instead of
// being silently ignored. `with` arrives with the run-time step-output
// substitution applied; the action parsers still apply matrix/inputs.
func planUses(p *Plan, sp *StepPlan, uses string, with map[string]string, env map[string]string) {
	action := strings.ToLower(strings.TrimSpace(uses))
	base := action
	if at := strings.IndexByte(base, '@'); at >= 0 {
		base = base[:at]
	}
	switch base {
	case "actions/checkout":
		sp.Kind = "checkout"
		depth := "1"
		if v, ok := with["fetch-depth"]; ok && strings.TrimSpace(v) == "0" {
			depth = ""
		}
		sp.Run = checkoutScript(p, depth)
		env["SPINDLE_JOB_TOKEN"] = "" // value supplied via --env NAME
		sp.EnvFrom = append(sp.EnvFrom, "SPINDLE_JOB_TOKEN")
		sp.Argv = execArgv(p, p.Workspace, env, sp.EnvFrom, []string{"sh", "-e", "-c", sp.Run})
	case "actions/upload-artifact":
		step, err := parseUploadArtifact(p, with)
		if err != nil {
			sp.Kind = "unsupported"
			sp.SkipReason = err.Error()
			return
		}
		sp.Kind = "artifact-upload"
		sp.Artifact = step
	case "actions/download-artifact":
		step, err := parseDownloadArtifact(p, with)
		if err != nil {
			sp.Kind = "unsupported"
			sp.SkipReason = err.Error()
			return
		}
		sp.Kind = "artifact-download"
		sp.Artifact = step
	default:
		sp.Kind = "unsupported"
		sp.SkipReason = fmt.Sprintf("unsupported action %q: spindle runs the built-in set (actions/checkout, actions/upload-artifact, actions/download-artifact), repository-local actions (uses: ./<path>) and docker:// images; there is no marketplace for remote actions", uses)
	}
}

// checkoutScript builds the in-container clone (15 §4): git, the job token as
// an askpass-style credential helper (never written into .git/config), the
// exact run commit.
//
// A failed fetch does NOT end at git's bare `exit 128`: the script prints git's
// own output verbatim (the evidence), then a named diagnostic that states the
// URL tried, the reachability requirement and what to configure. The first-run
// trap this closes is an unreachable checkout origin (a host-only name such as
// walgit.localhost, or a proxy address), which is otherwise indistinguishable
// from any other git failure in the log.
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
	fetch := "git -c credential.helper=" + shQuote(helper) + " fetch -q " + depthFlag + "origin " + shQuote(p.CheckoutSHA)
	lines := []string{
		"set -e",
		"cd " + shQuote(p.Workspace),
		"git init -q",
		"git remote remove origin >/dev/null 2>&1 || true",
		"git remote add origin " + shQuote(p.CheckoutURL),
		"if checkout_out=$(" + fetch + " 2>&1); then",
		"\t:",
		"else",
		"\trc=$?",
		"\tprintf '%s\\n' \"$checkout_out\" >&2",
		"\tcase \"$checkout_out\" in",
		"\t*\"Could not resolve\"*|*\"unable to access\"*|*\"Connection refused\"*|*\"Failed to connect\"*|*\"Network is unreachable\"*|*\"No route to host\"*|*\"Temporary failure in name resolution\"*|*\"timed out\"*)",
	}
	lines = append(lines, printfLines(p.checkoutHint(true))...)
	lines = append(lines, "\t\t;;")
	lines = append(lines, "\t*)")
	lines = append(lines, printfLines(p.checkoutHint(false))...)
	lines = append(lines, "\t\t;;", "\tesac", "\texit \"$rc\"", "fi",
		"git checkout -q FETCH_HEAD",
		"echo \"checked out "+p.CheckoutSHA+"\"",
	)
	return strings.Join(lines, "\n")
}

// printfLines renders one stderr line per hint line.
func printfLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, "\t\tprintf '%s\\n' "+shQuote(line)+" >&2")
	}
	return out
}

// checkoutHint is the named checkout failure message. reachable selects the
// name/connection paragraph (git reported an unreachable URL) versus the
// generic "the fetch failed, see git's output above" line; both name the URL
// and the configuration that decides it.
func (p *Plan) checkoutHint(unreachable bool) []string {
	lines := []string{"", "spindle: checkout failed for " + p.CheckoutURL}
	if p.CheckoutOriginURL != "" && p.CheckoutOriginURL != p.CheckoutURL {
		lines = append(lines, "spindle:   (the payload named "+p.CheckoutOriginURL+", rewritten for the container)")
	}
	if unreachable {
		lines = append(lines,
			"spindle:   git reported a name or connection error (its output is above)",
			"spindle:   the checkout URL must resolve and be reachable from INSIDE the job container —",
			"spindle:   a host-only name (localhost, a *.localhost alias, or the server's own hostname)",
			"spindle:   resolves on the host but never there.",
			"spindle:   fix: set server.public_url (Settings -> Server -> Advanced -> public_url) to the",
			"spindle:   address job containers can reach, and/or start the runner with --server at one:",
			"spindle:   a host-socket daemon reaches the host as host.docker.internal, a bridge-only host",
			"spindle:   as its docker0 gateway (usually 172.17.0.1).",
		)
	} else {
		lines = append(lines,
			"spindle:   git's output above is the cause; the URL must also be reachable from inside",
			"spindle:   the job container (set server.public_url, or the runner's --server, to an",
			"spindle:   address containers can reach).",
		)
	}
	return lines
}

// checkoutNotice is the once-per-job start line: the URL actually tried and
// the host:port it resolves to inside the job container (the resolved base an
// operator needs when debugging reachability).
func checkoutNotice(p *Plan) string {
	url := p.CheckoutURL
	endpoint := ""
	if u, err := neturl.Parse(url); err == nil && u.Host != "" {
		endpoint = u.Host
	}
	notice := "checkout from " + url
	if endpoint != "" {
		notice += ", reachable inside the job container as " + endpoint
	}
	if p.CheckoutOriginURL != "" && p.CheckoutOriginURL != url {
		notice += " (payload URL " + p.CheckoutOriginURL + " rewritten for the container)"
	}
	return notice
}

// credentialEnv are the values a step receives by NAME (`--env NAME` in argv,
// value in the docker client's environment) and never as argv text: the job
// token and the OIDC request token. A docker action step never carries either
// (its EnvFrom is empty), because the image is third-party code.
func credentialEnv(p *Plan, from []string) []string {
	if p == nil {
		return nil
	}
	var out []string
	if containsString(from, "SPINDLE_JOB_TOKEN") {
		out = append(out, "SPINDLE_JOB_TOKEN="+p.JobToken)
	}
	if p.IDTokenRequestToken != "" {
		if containsString(from, "SPINDLE_ID_TOKEN_REQUEST_TOKEN") {
			out = append(out, "SPINDLE_ID_TOKEN_REQUEST_TOKEN="+p.IDTokenRequestToken)
		}
		if containsString(from, "ACTIONS_ID_TOKEN_REQUEST_TOKEN") {
			out = append(out, "ACTIONS_ID_TOKEN_REQUEST_TOKEN="+p.IDTokenRequestToken)
		}
	}
	return out
}

// execArgv builds `docker exec` for one step: the step environment first, then
// the NAME-only credentials (their values come from credentialEnv). Secret
// values are appended as `--env NAME` (the value is supplied through the docker
// client's own environment, so it never appears in argv) and masked in the log
// stream.
func execArgv(p *Plan, workdir string, env map[string]string, from []string, cmd []string) []string {
	argv := []string{"exec", "--workdir", workdir}
	keys := make([]string, 0, len(env))
	for k := range env {
		if k == "SPINDLE_JOB_TOKEN" || k == "SPINDLE_ID_TOKEN_REQUEST_TOKEN" || k == "ACTIONS_ID_TOKEN_REQUEST_TOKEN" {
			continue // supplied by name below (value from the docker client's env)
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
// (single/double quotes group, whitespace separates). It is the policy's own
// splitter (threads.SplitOptions), so the string docker receives and the string
// the allowlist checked are tokenised identically.
func splitOptions(s string) []string {
	return threads.SplitOptions(s)
}

// checkWorkflowContainer applies the server's container option/mount allowlist
// at plan time (defence in depth: the job must not start even if it was created
// by an older or bypassed server). field is the record prefix the diagnostic
// names ("container" or "services.<id>").
func checkWorkflowContainer(field, options string, volumes []string) error {
	if r := threads.CheckWorkflowContainerOptions(field, options, volumes); r != nil {
		return fmt.Errorf("%s", r.Reason)
	}
	return nil
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
