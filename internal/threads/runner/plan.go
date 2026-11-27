package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	neturl "net/url"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	// ServerURL is the spindle origin the runner was configured with; it is the
	// fallback when the job payload carries no canonical origin (the server's
	// server.public_url / request-derived base wins because it is the address job
	// containers can reach — PAR-111).
	ServerURL string
	// RunnerName is the registered runner's name (`runner.name`).
	RunnerName string
	// Caps is the server's artifact cap advertisement (ExecInput.Caps).
	Caps threads.ArtifactCaps
	// Policy is the runner operator's container grants (PAR-179).
	Policy threads.ContainerPolicy
	// CacheCaps is the server's cache cap advertisement (ExecInput.CacheCaps).
	CacheCaps threads.CacheCaps
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
	// NetworkMode is the docker network the job, its services and docker
	// actions join: the per-job network unless the operator's container policy
	// names one (PAR-179).
	NetworkMode string
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
	// ServerURL is the canonical spindle origin (no trailing slash) the job
	// advertises as GITHUB_SERVER_URL/FORGEJO_SERVER_URL (GITHUB_API_URL is
	// <ServerURL>/api/v1). Empty when neither the payload nor the runner was
	// configured with one, in which case the variables are simply absent.
	ServerURL string

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
	// GitHub is the resolved `github.*` context of this job (docs/threads.md
	// §3.6): the runner built-ins, captured BEFORE any authored `env:` overlay
	// so a workflow that sets GITHUB_SHA in `env:` cannot change what
	// `${{ github.sha }}` means, plus the run's actor and the documented
	// `github.event.*` projection. It is the one source of the
	// `github.<field>` substitution and of a step `if:` reading it.
	GitHub map[string]string
	// Vars is the resolved `vars.*` context (repository variables override
	// organization variables). Values are masked like secrets.
	Vars map[string]string
	// Runner is the resolved `runner.*` context (PAR-034).
	Runner map[string]string
	// EventJSON is the bounded event payload written verbatim to
	// $GITHUB_EVENT_PATH before the first step.
	EventJSON []byte
	// Needs is the folded output map of the job's `needs:` dependencies (the
	// server materializes it at claim time). Step fields read it as
	// `${{ needs.<job>.outputs.<name> }}`; NeedsResults carries the folded
	// results for `${{ needs.<job>.result }}`.
	Needs        map[string]map[string]string
	NeedsResults map[string]string
	// OutputTemplates are the job's declared `outputs:` templates (matrix and
	// inputs already substituted), evaluated against the collected step
	// outputs when the job finishes (docs/threads.md §3.9).
	OutputTemplates map[string]string
	// ArtifactCaps are the server's upload caps (zero = the service defaults).
	ArtifactCaps threads.ArtifactCaps
	// Policy is the runner operator's container grants (PAR-179). The zero
	// value is the fail-closed default.
	Policy threads.ContainerPolicy
	// DockerHost is the docker endpoint granted to the JOB container only
	// ("" = none). It is never given to a docker action, a service or a step
	// environment built by a workflow.
	DockerHost string
	// CacheCaps are the server's cache limits (zero = the service defaults).
	CacheCaps threads.CacheCaps
	// deferred are the `actions/cache` saves performed at the end of a green
	// job (docs/threads.md §3.4c). Per-plan, so concurrent jobs never share it.
	deferred []*CacheStep
	// Actions are the repository-local and resolved remote action definitions
	// embedded in the job payload at creation (docs/threads.md §3.4a/§3.4d).
	// They are pinned to the run commit (local) or to a resolved commit + content
	// digest (remote); the runner never reads a repository or the network for an
	// action, so an expansion can never pick up a file from another ref.
	Actions map[string]threads.ActionSpec
	// HashFiles is the job container's workspace walker (PAR-041), installed by
	// the executor once the container exists. Nil at plan time (the preview
	// leaves those spans authored).
	HashFiles func(patterns []string) (string, error)
	// StepOutcomes/StepConclusions are the steps resolved so far, for a field
	// expression that reads `steps.<id>.outcome`/`.conclusion` (PAR-038). The
	// executor refreshes them before each ResolveStep.
	StepOutcomes    map[string]string
	StepConclusions map[string]string
	// Posts are the action post hooks (PAR-071), run in reverse after the last
	// main step — including after a hard failure.
	Posts []PostPlan
	// exec is the per-execution mutable state (materialized actions, ran steps,
	// built images). A pointer keeps Plan copyable and one job's execution from
	// sharing state with another.
	exec *planExec
}

// planExec is the per-execution state of one plan.
type planExec struct {
	mu            sync.Mutex
	ran           map[int]bool
	materialized  map[string]bool
	builtImages   []string
	loggedActions map[string]bool
	probedNode    bool
	nodeVersion   string
	nodeErr       error
}

// markRan records that the step at OutputIndex started executing (it may have
// failed; a post hook still runs for a failed main).
func (p *Plan) markRan(index int) {
	if p == nil || p.exec == nil {
		return
	}
	p.exec.mu.Lock()
	p.exec.ran[index] = true
	p.exec.mu.Unlock()
}

// wasRan reports whether the step at OutputIndex started.
func (p *Plan) wasRan(index int) bool {
	if p == nil || p.exec == nil {
		return false
	}
	p.exec.mu.Lock()
	defer p.exec.mu.Unlock()
	return p.exec.ran[index]
}

// markMaterialized reports whether dir was already extracted; the first caller
// gets false and is responsible for extracting it.
func (p *Plan) markMaterialized(dir string) bool {
	if p == nil || p.exec == nil {
		return true
	}
	p.exec.mu.Lock()
	defer p.exec.mu.Unlock()
	if p.exec.materialized[dir] {
		return true
	}
	p.exec.materialized[dir] = true
	return false
}

// addBuiltImage records a Dockerfile action image for job-end cleanup.
func (p *Plan) addBuiltImage(image string) {
	if p == nil || p.exec == nil {
		return
	}
	p.exec.mu.Lock()
	p.exec.builtImages = append(p.exec.builtImages, image)
	p.exec.mu.Unlock()
}

// builtImages returns the images built for this job.
func (p *Plan) builtImageList() []string {
	if p == nil || p.exec == nil {
		return nil
	}
	p.exec.mu.Lock()
	defer p.exec.mu.Unlock()
	return append([]string{}, p.exec.builtImages...)
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
	Shell            string // the resolved `shell:` ("" is the image's sh)
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
	// Composite is set on a step that expands into a composite action:
	// the inner steps run in order and the action's declared outputs are
	// evaluated afterwards.
	Composite *CompositePlan
	// Node is set on a JavaScript action step (PAR-071): the action's pinned
	// file set, its main program, and its pre/post hooks.
	Node *NodePlan
	// ThirdParty marks code fetched from an action source (a remote action's
	// node program, or the run steps inside a remote composite). It receives
	// no implicit job token, no OIDC request token and no repository secrets:
	// only its declared `with:`/`env:` interface (D-5).
	ThirdParty bool
	// Origin is the resolution record of a fetched action (source, ref, pinned
	// commit, content digest): the executor states it once per job in the log.
	Origin *threads.ActionOrigin
	// Docker is set on a docker:// step (and a local docker action): the image
	// actually run (after the runner's prefix rewrite) and the metadata's
	// entrypoint/args/env.
	Docker *DockerPlan
	// OutputPath is the in-container file this step may write outputs to
	// ($SPINDLE_OUTPUT, aliased as $GITHUB_OUTPUT). Only a step with an id is
	// collected (docs/threads.md §3.9).
	OutputPath string
	// SummaryPath is the in-container file this step may write its step
	// summary to ($GITHUB_STEP_SUMMARY, PAR-092). It is collected after every
	// step, capped per step (docs/threads.md §3.9a).
	SummaryPath string
	// ScriptFile is the in-container script path for a CUSTOM `shell:` command
	// (one containing `{0}`). Empty for a named shell, whose script travels on
	// the interpreter's command line (PAR-079).
	ScriptFile string
	// EnvFile/PathFile are this step's command files ($GITHUB_ENV and
	// $GITHUB_PATH, with the Forgejo alias names). Their effects reach LATER
	// steps of the same job only (docs/threads.md §3.10). Both are empty for a
	// docker:// action, whose image is third-party code and never receives the
	// job's environment layer.
	EnvFile  string
	PathFile string
	// StepEnv are the names this step's own `env:` block sets. A command-file
	// value never overrides that most specific layer.
	StepEnv []string
	// EnvSecret maps a container variable onto the secret name whose value it
	// receives at exec time: `env: FORGEJO_TOKEN: ${{ secrets.GITHUB_TOKEN }}`
	// delivers the alias BY NAME, so no secret value ever reaches argv.
	EnvSecret map[string]string
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
	// Cache is set for the built-in cache actions (PAR-095): actions/cache,
	// actions/cache/restore and actions/cache/save.
	Cache *CacheStep
	// Template holds the step's fields after the plan-time matrix/inputs
	// substitution and before the run-time `steps.<id>.outputs.*`
	// substitution. The executor re-resolves the step against the outputs
	// collected so far before it runs (ResolveStep).
	Template *StepTemplate
	// Background markers and the control-step directives (PAR-080).
	Background bool
	Wait       string
	WaitAll    bool
	Cancel     string
	// Parallel is the resolved member list of a `parallel:` group. The group's
	// Kind is "parallel"; members run concurrently and are reported in order.
	Parallel []StepPlan
}

// CompositePlan is one expanded composite action.
type CompositePlan struct {
	// Path is the action directory as authored (repository-relative for a
	// local action).
	Path string
	// Dir is the absolute in-container directory the action's own steps see
	// as `github.action_path` (`<workspace>/<path>` for a local action,
	// `/spindle/remote-actions/<digest>` for a remote one).
	Dir string
	// Files is the fetched content a remote action must materialize before
	// its steps run (empty for a local action: the checkout already has it).
	Files []threads.ActionFile
	// Steps are the action's steps, expanded recursively in order.
	Steps []StepPlan
	// Outputs maps the action's declared output names to the templates
	// evaluated over its own steps' outputs.
	Outputs map[string]string
	// Inputs are the caller's resolved input values: the action's steps read
	// them as `${{ inputs.<name> }}` (PAR-074).
	Inputs map[string]string
	// ThirdParty marks a remote composite: its run steps get no implicit
	// credential.
	ThirdParty bool
}

// NodePlan is one JavaScript action invocation (PAR-071).
type NodePlan struct {
	// Dir is the absolute in-container action directory.
	Dir string
	// Main is the program file, relative to Dir.
	Main string
	// Using is node20 or node24 (the minimum runtime the action declares).
	Using string
	// StatePath is this action's $GITHUB_STATE file, shared by pre, main and
	// post.
	StatePath string
	// Pre/Post are the hook programs and their conditions ("" = always()).
	Pre, Post     string
	PreIf, PostIf string
	// Files is the fetched content the runner materializes before the pre
	// hook (or the main program) runs.
	Files []threads.ActionFile
}

// PostPlan is one action post hook: the step to run at the end of the job and
// the OutputIndex of the main step that must have started for it to run. Posts
// run in reverse order after the last main step, including after a failure.
type PostPlan struct {
	Step StepPlan
	Gate int
}

// DockerPlan is one docker action invocation's metadata.
type DockerPlan struct {
	// Path is the local action directory ("" for an inline docker:// step).
	Path string
	// Dir is the absolute in-container action directory (remote actions).
	Dir string
	// Files is the fetched content (remote actions): a Dockerfile action is
	// built from it on the runner host before the container starts.
	Files []threads.ActionFile
	// Dockerfile is the action-relative Dockerfile path when this action
	// builds its own image (PAR-072); "" for a registry image.
	Dockerfile string
	// ThirdParty marks fetched action code (registry image or Dockerfile).
	ThirdParty bool
	// Origin is the fetched action's resolution record (nil for docker:// and
	// local actions).
	Origin *threads.ActionOrigin
	// Image is the reference actually run (after --image-prefix);
	// RequestedImage is what the workflow or metadata named.
	Image          string
	RequestedImage string
	// ImageIndex is the position of the image in the `docker run` argv, so a
	// built Dockerfile action can substitute the tag after `docker build`.
	ImageIndex int
	// Entrypoint/Args/Env come from the action metadata (docker actions only)
	// and are empty for an inline docker:// step: the image's own entrypoint
	// and command are used then.
	Entrypoint []string
	Args       []string
	Env        map[string]string
}

// StepTemplate is a step's authored fields. Substitution happens when the step
// is resolved (workflow.Substitute, one pass): at plan time for the preview
// (step/needs outputs deferred), and again for real immediately before the step
// runs, because each field may read an earlier step's outputs.
type StepTemplate struct {
	Name             string
	Run              string
	If               string
	Shell            string
	Uses             string
	WorkingDirectory string
	// With/Env are the authored `with:`/`env:` values (a composite action's
	// inner step additionally carries its caller's env layer, see Inherited).
	With map[string]string
	Env  map[string]string
	// Inherited is the caller's environment layer for a composite action's
	// inner step: it resolves first, so the step's own `env:` reads the layer
	// below it.
	Inherited map[string]string
	// ActionPath is the in-container directory of the composite action this
	// step belongs to (PAR-074): the value `${{ github.action_path }}` has in a
	// composite action's OWN steps. Empty for a workflow step, where the parser
	// refuses the reference.
	ActionPath string
	// ActionInputs are the enclosing action's input values: the `inputs.*`
	// context inside a composite action's steps (PAR-074). Empty for a workflow
	// step, where `inputs.*` is the dispatch input set.
	ActionInputs map[string]string
	// ThirdParty marks a step inside a fetched action: no implicit credential.
	ThirdParty bool
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
	if err := threads.ValidateContainerPolicy(in.Policy); err != nil {
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
	// The canonical origin: the payload's (server.public_url, else the origin the
	// runner reached) wins over the runner's own --server, because it is the
	// address JOB CONTAINERS can use. A malformed value never reaches the
	// environment (a job that curls $GITHUB_SERVER_URL must not get garbage).
	serverBase := canonicalServerURL(job.ServerURL)
	if serverBase == "" {
		serverBase = canonicalServerURL(in.ServerURL)
	}
	buildEnv := builtinEnv(job, ws, serverBase, matrix)
	runnerName := strings.TrimSpace(in.RunnerName)
	if runnerName == "" {
		runnerName = "spindle-runner"
	}
	// Runner context and the event file (PAR-033/PAR-034). The path variables
	// are set before the container exists, so every step (and the shell that
	// runs it) sees the same file.
	for k, v := range map[string]string{
		"RUNNER_NAME":                runnerName,
		"RUNNER_OS":                  "Linux",
		"RUNNER_ARCH":                runnerContext(buildEnv, runnerName)["arch"],
		"RUNNER_TEMP":                RunnerTemp,
		"RUNNER_TOOL_CACHE":          RunnerToolCache,
		"RUNNER_ENVIRONMENT":         "self-hosted",
		"RUNNER_DEBUG":               "0",
		"GITHUB_EVENT_PATH":          EventPath,
		"FORGEJO_EVENT_PATH":         EventPath,
		"SPINDLE_EVENT_PATH":         EventPath,
		"SPINDLE_RUNNER_NAME":        runnerName,
		"SPINDLE_RUNNER_OS":          "Linux",
		"SPINDLE_RUNNER_ARCH":        runnerContext(buildEnv, runnerName)["arch"],
		"SPINDLE_RUNNER_TEMP":        RunnerTemp,
		"SPINDLE_RUNNER_TOOL_CACHE":  RunnerToolCache,
		"SPINDLE_RUNNER_ENVIRONMENT": "self-hosted",
	} {
		buildEnv[k] = v
	}
	// The `github.*` context is captured from the runner's own built-ins BEFORE
	// an authored `env:` overlay: the context is the run's identity, not a
	// variable a workflow may redefine (docs/threads.md §3.6).
	github := githubContext(buildEnv)
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
	mask := make([]string, 0, len(job.Secrets)+len(job.Mask)+2)
	for name, value := range job.Secrets {
		// The token aliases are reserved: a stored secret of that name is ignored
		// so one variable can never carry two values (docs/threads.md §5.3). Its
		// value is still masked (the server puts every secret value in Mask).
		if reservedSecretName(name) {
			continue
		}
		secretEnv[name] = value
	}
	mask = append(mask, job.Mask...)
	// EVERY secret value the plan can substitute is registered here, exactly
	// like an `env:`-delivered one: `${{ secrets.X }}` inside a `run:` value is
	// spliced into the script, so the log stream must mask it even if the
	// payload's own mask list omitted it (the plan is self-contained).
	for _, value := range secretEnv {
		if value != "" {
			mask = append(mask, value)
		}
	}
	// The attempt token is a bearer credential, and it is now also exposed as
	// GITHUB_TOKEN/FORGEJO_TOKEN (§5.3), so a workflow echoing it must be masked
	// exactly like a repository secret.
	if in.AttemptToken != "" {
		mask = append(mask, in.AttemptToken)
	}
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

	eventFields := threads.EventFieldsOf(job.EventPayload)
	for k, v := range eventFields {
		github[k] = v
	}
	eventJSON, eerr := threads.EncodeEventPayload(job.EventPayload)
	if eerr != nil {
		return nil, eerr
	}
	runnerCtx := runnerContext(buildEnv, runnerName)
	networkMode := names.Network
	if in.Policy.Network != "" {
		networkMode = in.Policy.Network
	}
	p := &Plan{
		JobRef: job.JobRef, Attempt: job.Attempt,
		Names: names, Workspace: ws, Image: image, RequestedImage: requestedImage,
		exec:        &planExec{ran: map[int]bool{}, materialized: map[string]bool{}},
		NetworkMode: networkMode,
		Images:      in.Images,
		Policy:      in.Policy,
		DockerHost:  strings.TrimSpace(in.Policy.DockerHost),
		CheckoutSHA: job.Commit, Secrets: secretEnv, Mask: mask,
		ServerURL:           serverBase,
		JobToken:            in.AttemptToken,
		IDTokenRequestToken: idTokenRequestToken,
		Matrix:              matrix, Inputs: inputs, Env: jobEnv, GitHub: github,
		Vars: job.Vars, Runner: runnerCtx, EventJSON: eventJSON,
		Needs:        job.NeedsOutputs,
		NeedsResults: job.NeedsResults,
		ArtifactCaps: in.Caps,
		CacheCaps:    in.CacheCaps,
		Actions:      spec.Actions,
	}
	// The job's output templates stay as authored: every reference (github,
	// matrix, inputs, env, secrets, step and needs outputs) is resolved in one
	// pass when the job finishes (docs/threads.md §3.6/§3.9).
	if len(spec.Outputs) > 0 {
		p.OutputTemplates = make(map[string]string, len(spec.Outputs))
		for name, tmpl := range spec.Outputs {
			p.OutputTemplates[name] = tmpl
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
	if err := validateBackgroundPlan(p.Steps); err != nil {
		return nil, err
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

// githubContext maps the runner's built-in GITHUB_* environment onto the closed
// `github.*` context (docs/threads.md §3.6). It is the ONE source of the
// context: substitution, a step `if:` and the concurrency/name expansions on
// the server all read the same field set, so one reference cannot mean two
// values. A field the environment does not carry (no actor, no server
// addresses) is the empty string — an absent supported value expands empty.
func githubContext(env map[string]string) map[string]string {
	return map[string]string{
		"ref":        env["GITHUB_REF"],
		"ref_name":   env["GITHUB_REF_NAME"],
		"sha":        env["GITHUB_SHA"],
		"event_name": env["GITHUB_EVENT_NAME"],
		"workflow":   env["GITHUB_WORKFLOW"],
		"job":        env["GITHUB_JOB"],
		"repository": env["GITHUB_REPOSITORY"],
		"run_id":     env["GITHUB_RUN_ID"],
		"actor":      env["GITHUB_ACTOR"],
		"server_url": env["GITHUB_SERVER_URL"],
		"api_url":    env["GITHUB_API_URL"],
	}
}

// secretValues is the secrets context of this job: the decrypted repository and
// environment secrets, plus the reserved token aliases (GitHub's own spelling
// and Forgejo's), which can never be shadowed by a stored secret of the same
// name (docs/threads.md §5.3). An untrusted run's map is empty, so every
// reference reads empty — GitHub's rule, never the literal text.
func (p *Plan) secretValues() map[string]string {
	out := make(map[string]string, len(p.Secrets)+2)
	for name, value := range p.Secrets {
		out[name] = value
	}
	if p.JobToken != "" {
		out[JobTokenSecretName] = p.JobToken
		out[forgejoTokenSecret] = p.JobToken
	}
	return out
}

// githubValues is the plan's resolved `github.*` context (see githubContext).
// A hand-built plan (a test or a helper that never went through BuildPlan) gets
// the same map derived from its environment layer.
func (p *Plan) githubValues() map[string]string {
	if len(p.GitHub) > 0 {
		return p.GitHub
	}
	return githubContext(p.Env)
}

// jobRef is the run's `GITHUB_REF` and `GITHUB_REF_NAME`, in the one spelling
// every consumer agrees on: a branch push is `refs/heads/<branch>`, a tag push
// is `refs/tags/<tag>` and its short name is the tag (never the branch, which a
// tag run leaves empty), and a pull request is its BASE branch (the payload's
// `Ref` may be `refs/pull/<n>/head`, which is not what a job reads — the same
// rule the job OIDC token's `ref` claim applies). A payload with neither a ref
// nor a branch answers two empty strings, never a malformed `refs/heads/`.
func jobRef(job *threads.FetchResult) (ref, name string) {
	if job == nil {
		return "", ""
	}
	branch := strings.TrimSpace(job.Branch)
	full := strings.TrimSpace(job.Ref)
	// `Ref` is honoured only for a non-PR run and only when it is a real ref: an
	// older server may send neither, and a PR's ref is its base branch.
	if job.Event == threads.EventPullReq || !strings.HasPrefix(full, "refs/") {
		full = ""
	}
	if full == "" {
		if branch == "" {
			return "", ""
		}
		full = "refs/heads/" + branch
	}
	if tag, ok := strings.CutPrefix(full, "refs/tags/"); ok {
		return full, tag
	}
	if branch != "" {
		return full, branch
	}
	if name, ok := strings.CutPrefix(full, "refs/heads/"); ok {
		return full, name
	}
	return full, branch
}

// builtinEnv is the runner-provided environment (CI conventions: both the
// spindle names and the GitHub-compatible aliases workflows expect).
func builtinEnv(job *threads.FetchResult, ws, serverURL string, matrix map[string]string) map[string]string {
	ref, refName := jobRef(job)
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
		"GITHUB_REF_NAME":   refName,
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
	// The run's actor (PAR-058's github.actor) is exposed under GitHub's
	// spelling so `$GITHUB_ACTOR` and `${{ github.actor }}` agree; a payload
	// with no actor (an older server, a synthetic run) omits the variable and
	// the reference expands empty.
	if actor := strings.TrimSpace(job.Actor); actor != "" {
		env["GITHUB_ACTOR"] = actor
	}
	if serverURL != "" {
		env["SPINDLE_SERVER"] = serverURL
		// Server/API addresses (PAR-111): the configured external origin — never a
		// hardcoded host — so a proxied deployment works and the corpus's
		// `$GITHUB_SERVER_URL/$GITHUB_REPOSITORY.git` / `$GITHUB_API_URL/...`
		// commands reach spindle. GITHUB_API_URL is the REST root, as on GitHub.
		env["GITHUB_SERVER_URL"] = serverURL
		env["GITHUB_API_URL"] = serverURL + "/api/v1"
		env["FORGEJO_SERVER_URL"] = serverURL
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

// EventPath is where every job writes its event JSON. It lives in the
// container's /tmp (not the workspace), so a `git clean` cannot remove it.
const EventPath = "/tmp/spindle-event.json"

// runnerContext builds the `runner.*` context (PAR-034) from the runner's own
// identity and the fixed Linux/arch of a job container.
func runnerContext(env map[string]string, name string) map[string]string {
	osName := "Linux"
	arch := runtime.GOARCH
	switch arch {
	case "amd64":
		arch = "X64"
	case "arm64":
		arch = "ARM64"
	case "386":
		arch = "X86"
	case "arm":
		arch = "ARM"
	}
	return map[string]string{
		"os":          osName,
		"arch":        arch,
		"name":        name,
		"temp":        RunnerTemp,
		"tool_cache":  RunnerToolCache,
		"environment": "self-hosted",
		"debug":       env["RUNNER_DEBUG"],
	}
}

// RunnerTemp / RunnerToolCache are the in-container directories a job may
// write to and cache tools in; setup creates both (mode 0777, so a job user
// without root can write).
const (
	RunnerTemp      = "/tmp/spindle-temp"
	RunnerToolCache = "/tmp/spindle-tool-cache"
)

// canonicalServerURL normalises a configured spindle origin: an absolute
// http(s) URL with no trailing slash. Anything else is "" (the variables are
// then absent rather than wrong).
func canonicalServerURL(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimRight(raw, "/")
	if raw == "" {
		return ""
	}
	u, err := neturl.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return raw
}

// The reserved spellings of the job token: the environment names it is
// delivered under and the secret name it answers to. GITHUB_TOKEN is GitHub's
// spelling, FORGEJO_TOKEN Forgejo's, SPINDLE_JOB_TOKEN spindle's own.
const (
	JobTokenSecretName  = "GITHUB_TOKEN"
	forgejoTokenSecret  = "FORGEJO_TOKEN"
	spindleTokenEnvName = "SPINDLE_JOB_TOKEN"
)

// reservedSecretName reports whether a name is the job token's: a repository
// secret may not shadow (or be shadowed by) the automatic token, and is ignored
// at plan time so one variable never has two values.
func reservedSecretName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), JobTokenSecretName) ||
		strings.EqualFold(strings.TrimSpace(name), forgejoTokenSecret)
}

// jobTokenEnvNames are the environment names the job token is delivered under:
// the spindle spelling and the two platform aliases. They are delivered by NAME
// (the value travels in the docker client's environment, never argv) and masked
// in the log stream.
func jobTokenEnvNames() []string {
	return []string{spindleTokenEnvName, JobTokenSecretName, forgejoTokenSecret}
}

// secretValue resolves one `${{ secrets.<name> }}` reference: the reserved
// token aliases first (they can never be shadowed by a stored secret of the same
// name — GitHub refuses to create one, and a job must never authenticate with a
// look-alike), then the job's decrypted secret map (case-insensitive, as on
// GitHub). The second result reports whether the name exists at all; a missing
// secret resolves to "" exactly like GitHub's empty context value.
func (p *Plan) secretValue(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if reservedSecretName(name) {
		return p.JobToken, p.JobToken != ""
	}
	for key, value := range p.Secrets {
		if strings.EqualFold(key, name) {
			return value, true
		}
	}
	return "", false
}

// secretRefRe matches a `${{ secrets.<name> }}` reference that is the WHOLE
// value of a step `env:` entry — the only place a secret reference is resolved,
// precisely because resolving it here keeps the value out of argv.
var secretRefRe = regexp.MustCompile(`^\s*\$\{\{\s*secrets\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}\s*$`)

// exactSecretRef reports whether an authored `env:` value is exactly one secret
// reference, and which secret it names. Anything else (a prefix/suffix, several
// references, another context) is not resolved: splicing a secret value into a
// string would put it in the docker argv.
func exactSecretRef(value string) (string, bool) {
	m := secretRefRe.FindStringSubmatch(value)
	if m == nil {
		return "", false
	}
	return m[1], true
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
		"--network", p.NetworkMode,
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
		// The workflow's options and volumes pass the runner operator's policy
		// BEFORE a single argv element is produced: a refused job must not
		// start (docs/threads.md §8.7).
		if err := checkWorkflowContainer(p.Policy, "container", spec.Container.Options, spec.Container.Volumes); err != nil {
			return nil, err
		}
		argv = append(argv, splitOptions(spec.Container.Options)...)
		for _, v := range spec.Container.Volumes {
			argv = append(argv, "--volume", v)
		}
	}
	// The operator's docker-endpoint grant (PAR-179/D-8) reaches the JOB
	// container only. A unix socket is bind-mounted at the same path (a
	// tcp:// endpoint needs only the environment), and the grant wins over an
	// authored DOCKER_HOST. Docker-action containers and services never receive
	// it (dockerRunArgv excludes it and services get no job env).
	if p.DockerHost != "" {
		if sock, ok := strings.CutPrefix(p.DockerHost, "unix://"); ok {
			argv = append(argv, "--mount", "type=bind,source="+sock+",target="+sock)
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
	if p.DockerHost != "" {
		argv = append(argv, "--env", "DOCKER_HOST="+p.DockerHost)
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
	if err := checkWorkflowContainer(p.Policy, "services."+id, svc.Options, svc.Volumes); err != nil {
		return ServicePlan{}, err
	}
	argv := []string{
		"create", "--name", name,
		"--network", p.NetworkMode,
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
	// actionDir is the in-container directory of the composite action whose
	// own steps are being planned (`${{ github.action_path }}`, PAR-074); empty
	// for a workflow step.
	actionDir string
	// actionInputs are the enclosing action's resolved input values, the
	// `inputs.*` context for `${{ inputs.<name> }}` (PAR-074).
	actionInputs map[string]string
	// thirdParty marks steps inside a fetched (remote) action: no implicit
	// token, no OIDC request token, no repository secrets (D-5).
	thirdParty bool
}

// planStep resolves one authored step into one or more plans. A plain step is
// one plan; a local composite action becomes one composite plan holding its
// expanded inner steps (recursively), which the executor runs in order; a
// `parallel:` group becomes one group plan holding its members; a control step
// (`wait`/`wait-all`/`cancel`) becomes one directive. next is the flat execution
// index (unique output files for inner steps).
func planStep(p *Plan, index int, st threads.StepSpec, ctx expandCtx, next *int) ([]StepPlan, error) {
	if len(st.Parallel) > 0 {
		return planParallelStep(p, index, st, ctx, next)
	}
	if isControlStep(st) {
		return planControlStep(p, index, st, ctx)
	}
	uses := strings.TrimSpace(st.Uses)
	if threads.IsLocalAction(uses) {
		return planLocalAction(p, index, st, ctx, next)
	}
	if threads.IsRemoteAction(uses) {
		return planRemoteAction(p, index, st, ctx, next)
	}
	if image := threads.DockerUsesImage(uses); image != "" {
		return planDockerStep(p, index, st, &DockerPlan{RequestedImage: image}, copyStrings(st.With), ctx, next)
	}
	sp, err := stepPlan(p, index, st, ctx, next)
	if err != nil {
		return nil, err
	}
	return []StepPlan{sp}, nil
}

// isControlStep reports a step that only waits for or cancels background work.
func isControlStep(st threads.StepSpec) bool {
	return strings.TrimSpace(st.Wait) != "" || st.WaitAll || strings.TrimSpace(st.Cancel) != ""
}

// planControlStep plans a `wait`/`wait-all`/`cancel` directive. It executes no
// process, allocates no output file and therefore does not advance the flat
// index; it is resolved (name/if) and run by the executor against the job's
// background set.
func planControlStep(p *Plan, index int, st threads.StepSpec, ctx expandCtx) ([]StepPlan, error) {
	wait, cancel := strings.TrimSpace(st.Wait), strings.TrimSpace(st.Cancel)
	if wait != "" && (st.WaitAll || cancel != "") || st.WaitAll && cancel != "" {
		return nil, fmt.Errorf("step %d: wait, wait-all and cancel are mutually exclusive", index+1)
	}
	if strings.TrimSpace(st.Run) != "" || strings.TrimSpace(st.Uses) != "" {
		return nil, fmt.Errorf("step %d: a wait/cancel step runs no process, so run/uses is refused", index+1)
	}
	sp := StepPlan{
		Index: index, Parent: ctx.parent, Kind: "control",
		ID:              strings.ToLower(strings.TrimSpace(st.ID)),
		ContinueOnError: st.ContinueOnError,
		Wait:            wait, WaitAll: st.WaitAll, Cancel: cancel,
		Template: &StepTemplate{Name: st.Name, If: st.If},
	}
	return []StepPlan{sp}, nil
}

// planParallelStep plans a `parallel:` group: every member is an ordinary
// run/uses step, each with its own output file, and the group is one unit in
// the log and the job's step list. Nested groups and background/control members
// are refused by name (a two-level concurrency model is not documented, and a
// background member would have no job-end ordering).
func planParallelStep(p *Plan, index int, st threads.StepSpec, ctx expandCtx, next *int) ([]StepPlan, error) {
	if ctx.depth != 1 {
		return nil, fmt.Errorf("step %d: parallel groups are not accepted inside a composite action", index+1)
	}
	if st.Background || isControlStep(st) {
		return nil, fmt.Errorf("step %d: a parallel group cannot itself be background, wait or cancel", index+1)
	}
	if strings.TrimSpace(st.Run) != "" || strings.TrimSpace(st.Uses) != "" {
		return nil, fmt.Errorf("step %d: a parallel group takes only member steps, not run/uses", index+1)
	}
	group := StepPlan{
		Index: index, Parent: ctx.parent, Kind: "parallel",
		ID:              strings.ToLower(strings.TrimSpace(st.ID)),
		ContinueOnError: st.ContinueOnError,
		Template:        &StepTemplate{Name: st.Name, If: st.If},
	}
	for i, child := range st.Parallel {
		if child.Background || isControlStep(child) || len(child.Parallel) > 0 {
			return nil, fmt.Errorf("step %d: parallel member %d: a member is an ordinary run/uses step (background, wait/cancel and nested parallel are refused)", index+1, i+1)
		}
		if strings.TrimSpace(child.Run) == "" && strings.TrimSpace(child.Uses) == "" {
			return nil, fmt.Errorf("step %d: parallel member %d has neither run nor uses", index+1, i+1)
		}
		planned, err := planStep(p, index, child, ctx, next)
		if err != nil {
			return nil, fmt.Errorf("step %d: parallel member %d: %w", index+1, i+1, err)
		}
		group.Parallel = append(group.Parallel, planned...)
	}
	if len(group.Parallel) == 0 {
		return nil, fmt.Errorf("step %d: a parallel group has no member steps", index+1)
	}
	return []StepPlan{group}, nil
}

// validateBackgroundPlan enforces PAR-080's structure after planning: a
// background step is addressable, a control step names a background step that
// runs earlier, and neither appears inside a composite action (where the
// executor has no job-end ordering for it).
func validateBackgroundPlan(steps []StepPlan) error {
	background := map[string]bool{}
	var walk func(steps []StepPlan, inComposite bool) error
	walk = func(steps []StepPlan, inComposite bool) error {
		for _, st := range steps {
			if inComposite && (st.Background || st.Kind == "control" || st.Kind == "parallel") {
				return fmt.Errorf("step %d: background, wait/cancel and parallel steps are not accepted inside a composite action", st.Index+1)
			}
			switch st.Kind {
			case "control":
				if st.ID != "" {
					return fmt.Errorf("step %d: a wait/cancel step runs no process and cannot own an id", st.Index+1)
				}
				for _, ref := range []string{st.Wait, st.Cancel} {
					if ref == "" || strings.EqualFold(ref, "all") {
						continue
					}
					if !background[strings.ToLower(ref)] {
						return fmt.Errorf("step %d: names background step %q, which is not a background step started earlier", st.Index+1, ref)
					}
				}
			case "parallel":
				// Members are ordinary steps; nothing to validate here.
			default:
				if st.Background {
					if st.ID == "" {
						return fmt.Errorf("step %d: a background step needs an id so a later step can wait for or cancel it", st.Index+1)
					}
					if st.Kind != "run" && st.Kind != "docker" {
						return fmt.Errorf("step %d: background is accepted only on a run or docker step", st.Index+1)
					}
					background[st.ID] = true
				}
			}
			if st.Composite != nil {
				if err := walk(st.Composite.Steps, true); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(steps, false)
}

// planRemoteAction expands one embedded remote action (PAR-070/071/072/074).
// The server rewrote its `uses:` to RemoteActionPrefix+key and embedded the
// fetched, digest-pinned definition; a definition the payload does not carry is
// a named plan error (the job fails before any container starts).
func planRemoteAction(p *Plan, index int, st threads.StepSpec, ctx expandCtx, next *int) ([]StepPlan, error) {
	uses := strings.TrimSpace(st.Uses)
	def, ok := p.Actions[uses]
	if !ok {
		return nil, fmt.Errorf("step %d (uses: %s): remote action is not in the job payload (the run was created without its fetched definition)", index+1, uses)
	}
	if def.Origin != nil {
		// The resolution record is stated in the job log: the author sees the
		// commit and digest the action was pinned to.
		// (No stream here — the executor logs it when the step starts.)
	}
	switch def.Using {
	case "composite":
		return planCompositeStep(p, index, st, def, ctx, next)
	case "docker":
		return planRemoteDockerAction(p, index, st, def, ctx, next)
	case "node20", "node24":
		return planNodeAction(p, index, st, def, ctx, next)
	default:
		return nil, fmt.Errorf("step %d (uses: %s): remote action has unsupported using %q", index+1, uses, def.Using)
	}
}

// planRemoteDockerAction plans a docker action fetched from an action source:
// a registry image (image-gated here, exactly like a local docker action) or a
// Dockerfile the runner builds (PAR-072).
func planRemoteDockerAction(p *Plan, index int, st threads.StepSpec, def threads.ActionSpec, ctx expandCtx, next *int) ([]StepPlan, error) {
	uses := strings.TrimSpace(st.Uses)
	with, bad := actionWith(def, st.With)
	if len(bad) > 0 {
		return nil, fmt.Errorf("step %d (uses: %s): input %q is not declared by the action", index+1, uses, bad[0])
	}
	dp := &DockerPlan{
		Path: def.Path, Dir: def.Path, Files: def.Files,
		ThirdParty: true, Origin: def.Origin, RequestedImage: def.Image, Entrypoint: def.Entrypoint,
		Args: def.Args, Env: def.Env,
	}
	if def.Dockerfile {
		// A Dockerfile action is pinned by the action content digest, not by a
		// registry digest; the image gate has nothing to resolve yet. The built
		// image is local-only (never pushed), so there is no mutable reference.
		dp.Dockerfile, dp.RequestedImage = def.Image, ""
	}
	return planDockerStep(p, index, st, dp, with, ctx, next)
}

// planNodeAction plans a JavaScript action (PAR-071): an optional pre hook, the
// main program, and a post hook registered for the end of the job. The runtime
// is the job image's node — the action declares a minimum version, and the
// executor refuses by name when the image has none (never a silent skip).
func planNodeAction(p *Plan, index int, st threads.StepSpec, def threads.ActionSpec, ctx expandCtx, next *int) ([]StepPlan, error) {
	uses := strings.TrimSpace(st.Uses)
	with, bad := actionWith(def, st.With)
	if len(bad) > 0 {
		return nil, fmt.Errorf("step %d (uses: %s): input %q is not declared by the action", index+1, uses, bad[0])
	}
	if strings.TrimSpace(def.Main) == "" {
		return nil, fmt.Errorf("step %d (uses: %s): the action declares no main program", index+1, uses)
	}
	dir := def.Path
	if dir == "" {
		return nil, fmt.Errorf("step %d (uses: %s): the action has no directory", index+1, uses)
	}
	// The caller's env applies to the action; INPUT_* wins so an env block
	// cannot silently shadow an input (the composite rule).
	childEnv := withoutInputs(ctx.env)
	for k, v := range st.Env {
		childEnv[k] = v
	}
	for k, v := range inputEnv(def.Inputs, with) {
		childEnv[k] = v
	}
	name := strings.TrimSpace(st.Name)
	if name == "" {
		name = dir
		if def.Origin != nil {
			name = def.Origin.Repository
			if def.Origin.Path != "" {
				name = name + "/" + def.Origin.Path
			}
		}
	}
	statePath := fmt.Sprintf("%s/%d", threads.RemoteStateRoot, *next)
	var out []StepPlan
	if def.Pre != "" {
		preIdx := *next
		*next++
		pre := StepPlan{
			Index: index, OutputIndex: preIdx, Parent: ctx.parent,
			Name: name + " (pre)", Kind: "node", Uses: st.Uses, ThirdParty: true,
			ContinueOnError: st.ContinueOnError,
			If:              hookIf(st.If, def.PreIf),
			Node:            &NodePlan{Dir: dir, Main: def.Pre, Using: def.Using, StatePath: statePath, Files: def.Files},
			Template: &StepTemplate{Name: name + " (pre)", If: hookIf(st.If, def.PreIf),
				Uses: st.Uses, ActionPath: dir, Env: childEnv},
		}
		pre.OutputPath = outputFilePath(preIdx)
		pre.EnvFile, pre.PathFile = commandFilePaths(preIdx)
		resolved, err := resolveStep(p, pre, nil, false)
		if err != nil {
			return nil, fmt.Errorf("step %d (uses: %s) pre: %w", index+1, uses, err)
		}
		out = append(out, resolved)
	}
	mainIdx := *next
	*next++
	main := StepPlan{
		Index: index, OutputIndex: mainIdx, Parent: ctx.parent,
		ID: strings.ToLower(strings.TrimSpace(st.ID)), Name: name,
		Kind: "node", Uses: st.Uses, ThirdParty: true, Origin: def.Origin,
		ContinueOnError: st.ContinueOnError,
		Node:            &NodePlan{Dir: dir, Main: def.Main, Using: def.Using, StatePath: statePath, Files: def.Files},
		Template:        &StepTemplate{Name: name, If: st.If, Uses: st.Uses, ActionPath: dir, Env: childEnv},
	}
	if st.TimeoutMinutes > 0 {
		main.Timeout = time.Duration(st.TimeoutMinutes) * time.Minute
	}
	main.OutputPath = outputFilePath(mainIdx)
	main.SummaryPath = summaryFilePath(mainIdx)
	main.EnvFile, main.PathFile = commandFilePaths(mainIdx)
	resolved, err := resolveStep(p, main, nil, false)
	if err != nil {
		return nil, fmt.Errorf("step %d (uses: %s): %w", index+1, uses, err)
	}
	out = append(out, resolved)
	if def.Post != "" {
		post := StepPlan{
			Index: index, OutputIndex: *next, Parent: ctx.parent,
			Name: name + " (post)", Kind: "node", Uses: st.Uses, ThirdParty: true,
			Node: &NodePlan{Dir: dir, Main: def.Post, Using: def.Using, StatePath: statePath, Files: def.Files},
			Template: &StepTemplate{Name: name + " (post)", If: def.PostIf,
				Uses: st.Uses, ActionPath: dir, Env: childEnv},
		}
		post.OutputPath = outputFilePath(post.OutputIndex)
		*next++
		resolvedPost, perr := resolveStep(p, post, nil, false)
		if perr != nil {
			return nil, fmt.Errorf("step %d (uses: %s) post: %w", index+1, uses, perr)
		}
		p.Posts = append(p.Posts, PostPlan{Step: resolvedPost, Gate: mainIdx})
	}
	return out, nil
}

// hookIf combines a step condition with a pre-if/post-if. An empty condition is
// true; a status function (always(), success(), failure(), cancelled()) is a
// valid if-expression.
func hookIf(stepIf, hook string) string {
	s, h := strings.TrimSpace(stepIf), strings.TrimSpace(hook)
	switch {
	case s == "":
		return h
	case h == "":
		return s
	default:
		return "(" + s + ") && (" + h + ")"
	}
}

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
		if def.Origin != nil {
			name = def.Origin.Repository
			if def.Origin.Path != "" {
				name = name + "/" + def.Origin.Path
			}
		}
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
		Background: st.Background,
		Composite: &CompositePlan{Path: dir, Dir: containerDir(p.Workspace, dir), Outputs: def.Outputs,
			Files: def.Files, Inputs: with, ThirdParty: def.ThirdParty},
		Template: &StepTemplate{
			Name: name, If: st.If, Uses: st.Uses,
			With: copyStrings(st.With), ActionInputs: copyStrings(ctx.actionInputs),
		},
	}
	if def.ThirdParty {
		caller.ThirdParty = true
	}
	caller.Origin = def.Origin
	if st.TimeoutMinutes > 0 {
		caller.Timeout = time.Duration(st.TimeoutMinutes) * time.Minute
	}
	inner := expandCtx{depth: ctx.depth + 1, parent: parent, env: childEnv, workdir: base,
		actionDir: containerDir(p.Workspace, dir), actionInputs: with, thirdParty: ctx.thirdParty || def.ThirdParty}
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
	resolved, err := resolveStep(p, caller, nil, false)
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
	// A docker:// step (and only an inline one) accepts with.args/entrypoint:
	// they override the image's own command and are not inputs (PAR-072).
	dp.ImageIndex = -1
	image := ""
	if dp.Dockerfile == "" {
		image = RewriteImage(strings.TrimSpace(dp.RequestedImage), p.Images)
		if image == "" {
			return nil, fmt.Errorf("step %d: docker action %q has no image", index+1, st.Uses)
		}
		if p.Images.RequireDigest && !HasImageDigest(image) {
			return nil, fmt.Errorf("step %d (uses: %s): %s", index+1, st.Uses, requireDigestError("action", image))
		}
	}
	if dp.Path == "" {
		// An inline docker:// step: with.args / with.entrypoint are the
		// documented overrides (PAR-072), not action inputs.
		if raw, ok := with["args"]; ok {
			dp.Args = splitArgs(raw)
			delete(with, "args")
		}
		if raw, ok := with["entrypoint"]; ok {
			dp.Entrypoint = splitArgs(raw)
			delete(with, "entrypoint")
		}
	}
	if dp.Dockerfile != "" && image == "" {
		// The built tag is only known at execution time; the argv carries a
		// placeholder so the image's position is stable (and the image gate
		// already knows a Dockerfile action has no registry reference).
		image = dockerfilePlaceholder
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
		ContinueOnError: st.ContinueOnError, Docker: dp, Origin: dp.Origin,
		Background: st.Background,
		Template: &StepTemplate{
			Name:             st.Name,
			If:               st.If,
			Uses:             st.Uses,
			WorkingDirectory: containerDir(wd, st.WorkingDirectory),
			With:             copyStrings(with),
			Env:              copyStrings(env),
			Inherited:        copyStrings(ctx.env),
			ActionPath:       ctx.actionDir,
			ActionInputs:     copyStrings(ctx.actionInputs),
			ThirdParty:       ctx.thirdParty || dp.ThirdParty,
		},
	}
	sp.ThirdParty = ctx.thirdParty || dp.ThirdParty
	if st.TimeoutMinutes > 0 {
		sp.Timeout = time.Duration(st.TimeoutMinutes) * time.Minute
	}
	sp.OutputIndex = *next
	*next++
	sp.OutputPath = outputFilePath(sp.OutputIndex)
	resolved, err := resolveStep(p, sp, nil, false)
	if err != nil {
		return nil, err
	}
	// Remember where the image sits in the argv: a Dockerfile action's image
	// tag is only known after the build at execution time.
	if resolved.Docker != nil && resolved.Docker.Image != "" {
		for i, arg := range resolved.Argv {
			if arg == resolved.Docker.Image {
				resolved.Docker.ImageIndex = i
			}
		}
	}
	return []StepPlan{resolved}, nil
}

// dockerfilePlaceholder is the argv token a Dockerfile action's image occupies
// until `docker build` produces the tag at execution time.
const dockerfilePlaceholder = "spindle-local-dockerfile-action"

// splitArgs splits a with.args / with.entrypoint value into words, honouring
// single and double quotes (GitHub's docker:// rule). An unterminated quote
// takes the rest as one word rather than dropping it.
func splitArgs(raw string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	inWord := false
	for _, r := range raw {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == ' ' || r == '\t' || r == '\n':
			if inWord || cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord || cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
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
		Name:             st.Name,
		Run:              st.Run,
		If:               st.If,
		Shell:            st.Shell,
		Uses:             st.Uses,
		WorkingDirectory: wd,
		With:             copyStrings(st.With),
		Env:              copyStrings(env),
		Inherited:        copyStrings(ctx.env),
		ActionPath:       ctx.actionDir,
		ActionInputs:     copyStrings(ctx.actionInputs),
		ThirdParty:       ctx.thirdParty,
	}
	sp := StepPlan{
		ThirdParty:      ctx.thirdParty,
		Index:           index,
		Parent:          ctx.parent,
		ID:              strings.ToLower(strings.TrimSpace(st.ID)),
		ContinueOnError: st.ContinueOnError,
		Background:      st.Background,
		Template:        tmpl,
	}
	sp.OutputIndex = *next
	*next++
	sp.OutputPath = outputFilePath(sp.OutputIndex)
	sp.SummaryPath = summaryFilePath(sp.OutputIndex)
	sp.EnvFile, sp.PathFile = commandFilePaths(sp.OutputIndex)
	if st.TimeoutMinutes > 0 {
		sp.Timeout = time.Duration(st.TimeoutMinutes) * time.Minute
	}
	return resolveStep(p, sp, nil, false)
}

// ResolveStep re-resolves a planned step against the outputs collected so far,
// rebuilding its argv. The executor calls it immediately before a step runs; a
// reference that cannot be resolved is a named error and fails the step
// (docs/threads.md §3.9). Step outputs and the job's `needs:` outputs are both
// substituted (a field may read either).
func (p *Plan) ResolveStep(step StepPlan, outputs map[string]map[string]string) (StepPlan, error) {
	return resolveStep(p, step, outputs, true)
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
	return resolveStep(p, step, merged, true)
}

// resolveStep fills a step's resolved fields from its template and (re)builds
// the docker argv. outputs are the step/needs outputs collected so far; strict
// is false for the plan-time preview, where no step has run yet and an output
// reference stays authored (ResolveStep resolves it for real just before the
// step runs). Every substituted site is expanded in ONE pass (docs/threads.md
// §3.6).
func resolveStep(p *Plan, sp StepPlan, outputs map[string]map[string]string, strict bool) (StepPlan, error) {
	t := sp.Template
	if t == nil {
		return sp, nil // a hand-built StepPlan (tests, the artifact helpers)
	}
	// The environment layer at this step, lowest first: the job's layer
	// (runner built-ins, workflow, job and container `env`, already folded into
	// p.Env), the CALLER's layer for a composite action's inner step, then the
	// step's own `env:`. A step's own values resolve against the layers below
	// them, so `env.X` inside an `env:` value never reads a sibling or itself.
	lower := make(map[string]string, len(p.Env)+len(t.Inherited))
	for k, v := range p.Env {
		lower[k] = v
	}
	inherited, err := p.resolveEnvLayerInputs(t.Inherited, lower, outputs, strict, t.ActionPath, t.ActionInputs)
	if err != nil {
		return sp, err
	}
	for k, v := range inherited {
		lower[k] = v
	}
	env := make(map[string]string, len(p.Env)+len(t.Env)+2)
	for k, v := range p.Env {
		env[k] = v
	}
	values := make(map[string]string, len(lower)+len(t.Env))
	for k, v := range lower {
		values[k] = v
	}
	sp.EnvFrom = nil
	sp.EnvSecret = nil
	sp.StepEnv = sp.StepEnv[:0]
	for _, k := range sortedKeys(t.Env) {
		sp.StepEnv = append(sp.StepEnv, k)
		// `env: NAME: ${{ secrets.X }}` (the whole value) is delivered BY NAME at
		// exec time: the value would otherwise become an argv element, and a
		// secret must never appear in argv, docker inspect or .git/config. The
		// reference still resolves to its value for `${{ env.NAME }}`, which is
		// what `$NAME` means inside the container.
		if secretName, ok := exactSecretRef(t.Env[k]); ok {
			if sp.EnvSecret == nil {
				sp.EnvSecret = make(map[string]string, 1)
			}
			sp.EnvSecret[k] = secretName
			value, _ := p.secretValue(secretName)
			values[k] = value
			continue
		}
		// The authored value is flattened first (an authored newline would be a
		// typo); a substituted OUTPUT value keeps its newlines — it is one argv
		// element to docker, and a multi-line output is legitimate data.
		val, verr := p.expandValue(headerValue(t.Env[k]), lower, outputs, strict)
		if verr != nil {
			return sp, verr
		}
		env[k] = val
		values[k] = val
	}
	sort.Strings(sp.StepEnv)
	expand := func(s string) (string, error) {
		return p.expandValueAtInputs(s, values, outputs, strict, t.ActionPath, t.ActionInputs)
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
	// A control step and a parallel group run no command of their own: the
	// executor resolves their members/directives at execution time. Returning
	// here keeps the run/uses branch below from turning them into a `sh -c ""`.
	if sp.Kind == "control" || sp.Kind == "parallel" {
		return sp, nil
	}
	if sp.Run, err = expand(t.Run); err != nil {
		return sp, err
	}
	shell, err := expand(t.Shell)
	if err != nil {
		return sp, err
	}
	sp.Shell = strings.TrimSpace(shell)
	wd, err := expand(t.WorkingDirectory)
	if err != nil {
		return sp, err
	}
	sp.Uses = t.Uses
	sp.WorkingDirectory = containerDir(p.Workspace, wd)
	sp.Kind, sp.SkipReason, sp.Argv, sp.Artifact, sp.Cache = "", "", nil, nil, nil
	with := make(map[string]string, len(t.With))
	for _, k := range sortedKeys(t.With) {
		val, verr := expand(t.With[k])
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
	// A fetched action's code is third-party: it never receives the job token,
	// the OIDC request token or the repository secret set (D-5). Its declared
	// inputs and `env:` block are the only interface it has.
	thirdParty := sp.ThirdParty || t.ThirdParty
	if p.JobToken != "" && !isDocker && sp.Composite == nil && !thirdParty {
		// The token is delivered under every spelling a workflow expects:
		// SPINDLE_JOB_TOKEN, GitHub's GITHUB_TOKEN and Forgejo's FORGEJO_TOKEN —
		// one credential, aliased, never a second token (PAR-110). An authored
		// `env:`/job `env:` value for the same name wins: a workflow that sets
		// GITHUB_TOKEN to its own PAT (publish-github.yml does) must keep it.
		for _, name := range jobTokenEnvNames() {
			if _, authored := env[name]; authored {
				continue
			}
			if _, secret := sp.EnvSecret[name]; secret {
				continue // the authored test: already delivered by name
			}
			sp.EnvFrom = append(sp.EnvFrom, name)
		}
	}
	// The OIDC request token follows the job token's rule exactly: delivered to
	// repository-code steps by NAME (value from the docker client's
	// environment), never to a docker action image (third-party code), and
	// never in argv. The URL is already in the container env (builtinEnv).
	if p.IDTokenRequestToken != "" && !isDocker && sp.Composite == nil && !thirdParty {
		env["SPINDLE_ID_TOKEN_REQUEST_TOKEN"] = ""
		sp.EnvFrom = append(sp.EnvFrom, "SPINDLE_ID_TOKEN_REQUEST_TOKEN")
		env["ACTIONS_ID_TOKEN_REQUEST_TOKEN"] = ""
		sp.EnvFrom = append(sp.EnvFrom, "ACTIONS_ID_TOKEN_REQUEST_TOKEN")
	}
	// A composite action's own steps see the action directory under both
	// spellings: `${{ github.action_path }}` (substituted into the fields, PAR-074)
	// and the GITHUB_ACTION_PATH environment variable a script may read.
	if t.ActionPath != "" {
		env["GITHUB_ACTION_PATH"] = t.ActionPath
	}
	// Every step gets the output file GitHub-style; only a step with an id is
	// collected (an anonymous step's writes are discarded, as on GitHub).
	if sp.OutputPath != "" {
		env[OutputEnvVar] = sp.OutputPath
		env[OutputEnvAlias] = sp.OutputPath
	}
	// The step summary file (PAR-092): every repository-code step may write
	// markdown here; the runner collects it after the step and joins it into the
	// job summary. A docker:// action gets none — its image is third-party code
	// and the job summary is rendered on the job page.
	if sp.SummaryPath != "" && !isDocker && sp.Composite == nil {
		env[SummaryEnvVar] = sp.SummaryPath
		env[SummaryEnvAlias] = sp.SummaryPath
	}
	// The command files are pointed at a per-step path the same way. A docker://
	// action has none (its image never receives the job's environment layer, and
	// a value written into a third-party image would have to be trusted to come
	// back — the same rule as secrets).
	if sp.EnvFile != "" && !isDocker && sp.Composite == nil {
		env[EnvFileVar] = sp.EnvFile
		env[EnvFileAlias] = sp.EnvFile
	}
	if sp.PathFile != "" && !isDocker && sp.Composite == nil {
		env[PathFileVar] = sp.PathFile
		env[PathFileAlias] = sp.PathFile
	}
	switch {
	case sp.Node != nil:
		// A JavaScript action (PAR-071): the job image's node runs the action's
		// pinned program with its declared inputs. GITHUB_ACTION_PATH points at
		// the materialized action directory; GITHUB_STATE is the per-action
		// file pre/main/post share. The executor probes the runtime before the
		// first node step and refuses by name when the image has none.
		sp.Kind = "node"
		for name, val := range with {
			env[workflow.InputEnvName(name)] = val
		}
		env["GITHUB_ACTION_PATH"] = sp.Node.Dir
		env["GITHUB_ACTION"] = sp.Name
		env["GITHUB_STATE"] = sp.Node.StatePath
		cmd := []string{"node", sp.Node.Dir + "/" + sp.Node.Main}
		sp.Argv = execArgv(p, p.Workspace, env, sp.EnvFrom, sp.EnvSecret, cmd, !thirdParty)
	case sp.Composite != nil:
		// Expanded at plan time; the composite itself has no command. The
		// children were planned against this step's inputs; the resolved
		// values (a `with:` entry may read an earlier step's output) replace
		// the plan-time copy so `${{ inputs.* }}` inside the action is the
		// caller's actual value (PAR-074).
		sp.Kind = "composite"
		sp.Composite.Inputs = with
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
		cmd, needsFile, cerr := shellCommand(sp.Shell, sp.Run, stepScriptPath(sp.OutputIndex))
		if cerr != nil {
			sp.Kind = "unsupported"
			sp.SkipReason = cerr.Error()
			break
		}
		sp.ScriptFile = ""
		if needsFile {
			sp.ScriptFile = stepScriptPath(sp.OutputIndex)
		}
		sp.Argv = execArgv(p, sp.WorkingDirectory, env, sp.EnvFrom, sp.EnvSecret, cmd, !thirdParty)
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
		"--network", p.NetworkMode,
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
		// DOCKER_HOST is the runner operator's grant to the JOB container, and
		// a third-party action never inherits it (PAR-179/D-8).
		if k == "SPINDLE_JOB_TOKEN" || k == "SPINDLE_ID_TOKEN_REQUEST_URL" ||
			k == "ACTIONS_ID_TOKEN_REQUEST_URL" || k == "SPINDLE_ID_TOKEN_REQUEST_TOKEN" ||
			k == "ACTIONS_ID_TOKEN_REQUEST_TOKEN" || k == "DOCKER_HOST" {
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

// stepValues is the complete runtime value set of one substitution site
// (docs/threads.md §3.6): the resolved `github.*` context, the job's matrix
// cell and dispatch inputs, the environment layer at the site, the job's
// secrets, and — once a step has run — the step and needs outputs. strict is
// false for the plan-time preview, where no step has published anything yet.
func (p *Plan) stepValues(env map[string]string, outputs map[string]map[string]string, strict bool) workflow.TemplateValues {
	vals := workflow.TemplateValues{
		GitHub:          p.githubValues(),
		Runner:          p.Runner,
		Vars:            p.Vars,
		Matrix:          p.Matrix,
		Inputs:          p.Inputs,
		Env:             env,
		Secrets:         p.secretValues(),
		NeedResults:     p.NeedsResults,
		HashFiles:       p.HashFiles,
		StepOutcomes:    p.StepOutcomes,
		StepConclusions: p.StepConclusions,
	}
	if strict {
		vals.Steps = outputs
		vals.Needs = p.Needs
	}
	return vals
}

// substitute expands every supported `${{ … }}` reference in one pass
// (workflow.Substitute): substituted data is a value, never a template, so a
// secret whose body contains `${{ github.ref }}` stays exactly that text. An
// undeclared `steps`/`needs` output is a named error; every other absent
// supported value expands empty.
func (p *Plan) substitute(s string, vals workflow.TemplateValues) (string, error) {
	return workflow.Substitute(s, vals)
}

// expandValue expands one step/site field against the environment layer at that
// site (strict selects the execution rule; see resolveStep).
func (p *Plan) expandValue(s string, env map[string]string, outputs map[string]map[string]string, strict bool) (string, error) {
	return p.expandValueAt(s, env, outputs, strict, "")
}

// expandValueAt is expandValue with a per-step `github.action_path` override: a
// composite action's own steps read the action's in-container directory
// (PAR-074), and every other site has no such value (the parser refuses the
// reference outside an action's steps).
func (p *Plan) expandValueAt(s string, env map[string]string, outputs map[string]map[string]string, strict bool, actionPath string) (string, error) {
	return p.expandValueAtInputs(s, env, outputs, strict, actionPath, nil)
}

// expandValueAtInputs is expandValueAt with the enclosing action's inputs as
// the `inputs.*` context (PAR-074: a composite action's steps read the action's
// own inputs, not the workflow_dispatch input set).
func (p *Plan) expandValueAtInputs(s string, env map[string]string, outputs map[string]map[string]string, strict bool, actionPath string, inputs map[string]string) (string, error) {
	vals := p.stepValues(env, outputs, strict)
	if len(inputs) > 0 {
		vals.Inputs = inputs
	}
	if actionPath != "" {
		gh := make(map[string]string, len(vals.GitHub)+1)
		for k, v := range vals.GitHub {
			gh[k] = v
		}
		gh["action_path"] = actionPath
		vals.GitHub = gh
	}
	if strict {
		return p.substitute(s, vals)
	}
	return p.previewValue(s, vals)
}

// resolveEnvLayer resolves one environment layer's values against the layer
// below it. A whole-value `${{ secrets.X }}` entry reads the secret's value
// here; the delivery rule (by NAME, never argv) belongs to the step's own
// `env:` block and is applied by resolveStep.

// resolveEnvLayerInputs is resolveEnvLayer with the enclosing action's inputs
// as the `inputs.*` context.
func (p *Plan) resolveEnvLayerInputs(layer, lower map[string]string, outputs map[string]map[string]string, strict bool, actionPath string, inputs map[string]string) (map[string]string, error) {
	if len(layer) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(layer))
	for _, k := range sortedKeys(layer) {
		if name, ok := exactSecretRef(layer[k]); ok {
			out[k], _ = p.secretValue(name)
			continue
		}
		val, err := p.expandValueAtInputs(headerValue(layer[k]), lower, outputs, strict, actionPath, inputs)
		if err != nil {
			return nil, err
		}
		out[k] = val
	}
	return out, nil
}

// sortedKeys returns a map's keys in a deterministic order (substitution is
// per-key, but a stable order keeps the plan preview reproducible).
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// previewValue is the plan-time expansion of one field: everything a reference
// can resolve without a step having run, with the `steps`/`needs` output spans
// (and hashFiles, which needs the running container) left exactly as authored.
// Those are resolved by ResolveStep immediately before the step executes (the
// plan preview is not what runs).
func (p *Plan) previewValue(s string, vals workflow.TemplateValues) (string, error) {
	if !strings.Contains(s, "${{") {
		return s, nil
	}
	guarded, spans := deferOutputRefs(s)
	if len(spans) == 0 {
		return workflow.Substitute(s, vals)
	}
	out, err := workflow.Substitute(guarded, vals)
	if err != nil {
		return "", err
	}
	for i := len(spans) - 1; i >= 0; i-- {
		out = strings.ReplaceAll(out, outputSentinel(i), spans[i])
	}
	return out, nil
}

// deferOutputRefs replaces every `steps.<id>.outputs.<name>` /
// `needs.<job>.outputs.<name>` span in s with a sentinel that cannot occur in
// authored text, returning the guarded string and the original spans. Only the
// span boundaries are read here (the first `}}` after a `${{`, exactly as the
// template scanner does); whether the reference itself is well formed stays
// workflow.Substitute's call.
func deferOutputRefs(s string) (string, []string) {
	var spans []string
	var b strings.Builder
	for {
		i := strings.Index(s, "${{")
		if i < 0 {
			b.WriteString(s)
			return b.String(), spans
		}
		b.WriteString(s[:i])
		rest := s[i+3:]
		j := strings.Index(rest, "}}")
		if j < 0 {
			b.WriteString(s[i:]) // unterminated: Substitute reports it
			return b.String(), spans
		}
		span := s[i : i+3+j+2]
		lower := strings.ToLower(span)
		if isOutputRef(span) || strings.Contains(lower, "hashfiles(") ||
			strings.Contains(lower, "steps.") || strings.Contains(lower, "needs.") {
			b.WriteString(outputSentinel(len(spans)))
			spans = append(spans, span)
		} else {
			b.WriteString(span)
		}
		s = rest[j+2:]
	}
}

// outputSentinel is the placeholder one deferred span is rewritten to.
func outputSentinel(i int) string {
	return "\x00spindle-output-" + strconv.Itoa(i) + "\x00"
}

// isOutputRef reports whether a `${{ … }}` body is the four-segment
// steps/needs output form. It asks the template parser (OutputRefs) rather than
// re-deriving the shape, so this preview helper can never disagree with what
// the validator accepted.
func isOutputRef(span string) bool {
	refs, err := workflow.OutputRefs(span)
	return err == nil && len(refs) == 1
}

// planUses resolves the built-in action set (15 §2, §2.5): actions/checkout,
// actions/upload-artifact and actions/download-artifact. An unknown action is
// recorded as an unsupported step: it fails with a clear message instead of
// being silently ignored. `with` arrives fully substituted (every `${{ … }}`
// reference already resolved by resolveStep), so an action parser never
// re-expands a value it was handed.
func planUses(p *Plan, sp *StepPlan, uses string, with map[string]string, env map[string]string) {
	action := strings.ToLower(strings.TrimSpace(uses))
	base := action
	if at := strings.IndexByte(base, '@'); at >= 0 {
		base = base[:at]
	}
	switch base {
	case "actions/checkout":
		opts, err := checkoutOptions(with)
		if err != nil {
			sp.Kind = "unsupported"
			sp.SkipReason = fmt.Sprintf("%s (uses: %s)", err.Error(), uses)
			return
		}
		sp.Kind = "checkout"
		sp.Run = checkoutScript(p, opts)
		// The checkout fetches with the job token; the aliases are already in
		// EnvFrom (resolveStep), so the credential reaches the helper by NAME.
		if p.JobToken != "" && !containsString(sp.EnvFrom, "SPINDLE_JOB_TOKEN") {
			sp.EnvFrom = append(sp.EnvFrom, "SPINDLE_JOB_TOKEN")
		}
		sp.Argv = execArgv(p, p.Workspace, env, sp.EnvFrom, sp.EnvSecret, []string{"sh", "-e", "-c", sp.Run}, true)
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
	case cacheAction, cacheRestoreAction, cacheSaveAction:
		step, err := parseCacheStep(base, uses, with)
		if err != nil {
			sp.Kind = "unsupported"
			sp.SkipReason = err.Error()
			return
		}
		switch base {
		case cacheSaveAction:
			sp.Kind = "cache-save"
		case cacheRestoreAction:
			sp.Kind = "cache-restore"
		default:
			sp.Kind = "cache"
		}
		sp.Cache = step
	case "actions/setup-node", "actions/setup-go", "actions/setup-python", "actions/setup-java", "actions/setup-dotnet", "actions/setup-ruby":
		// PAR-096: a setup-* action is marketplace code (a JS action with its
		// own runtime), which this runner does not execute. The cache half IS
		// supported: the refusal names the exact actions/cache step that
		// replaces it, with the canonical key shape for that ecosystem.
		sp.Kind = "unsupported"
		sp.SkipReason = setupCacheRecipe(base, with)
	default:
		sp.Kind = "unsupported"
		sp.SkipReason = fmt.Sprintf("unsupported action %q: spindle runs the built-in set (actions/checkout, actions/upload-artifact, actions/download-artifact), repository-local actions (uses: ./<path>) and docker:// images; there is no marketplace for remote actions", uses)
	}
}

// checkoutInputs are the `with:` keys actions/checkout honours. Anything else is
// refused BY NAME (PAR-073): an input that does nothing is a typo or a missing
// feature, never something to ignore silently.
var checkoutInputs = []string{"fetch-depth", "fetch-tags", "path", "ref"}

// checkoutRefusals names the unsupported documented inputs and the spindle
// spelling of the same job, so a refusal is actionable.
var checkoutRefusals = map[string]string{
	"clean":               "every job starts on a fresh workspace volume, so there is nothing to clean",
	"persist-credentials": "the checkout authenticates with the job token in an in-memory credential helper and never writes credentials to .git/config",
	"repository":          "a cross-repository checkout needs a credential for that repository; run plain git fetch with your own token in a run step",
	"sparse-checkout":     "run git sparse-checkout in a run step after the checkout",
	"ssh-key":             "the checkout uses the job token over HTTP; use git with your own key in a run step",
	"submodules":          "run git submodule update --init --recursive in a run step after the checkout",
	"token":               "the checkout authenticates with the job token (no separate token input); a contents: read job clones read-only",
}

// checkoutOpts is the resolved `with:` of one actions/checkout step.
type checkoutOpts struct {
	// Depth is the --depth value; "" is a full clone (fetch-depth: 0).
	Depth string
	// Tags adds --tags to the fetch (fetch-tags: true); otherwise --no-tags is
	// passed explicitly so the behaviour never depends on git's config.
	Tags bool
	// Ref is the branch, tag or SHA to fetch; "" is the run commit.
	Ref string
	// Path is the checkout directory relative to the workspace; "" is the
	// workspace root.
	Path string
}

// checkoutOptions parses and validates actions/checkout's `with:` mapping.
func checkoutOptions(with map[string]string) (checkoutOpts, error) {
	keys := make([]string, 0, len(with))
	for k := range with {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !containsString(checkoutInputs, k) {
			hint := ""
			if hint = checkoutRefusals[k]; hint != "" {
				hint = ": " + hint
			}
			return checkoutOpts{}, fmt.Errorf("actions/checkout input %q is not supported (supported: %s)%s", k, strings.Join(checkoutInputs, ", "), hint)
		}
	}
	opts := checkoutOpts{Depth: "1"}
	if v, ok := with["fetch-depth"]; ok {
		v = strings.TrimSpace(v)
		if v == "0" {
			opts.Depth = ""
		} else if v != "" {
			// A depth is a positive integer; anything else (a negative, a word)
			// is a named error rather than git's own complaint.
			for _, r := range v {
				if r < '0' || r > '9' {
					return checkoutOpts{}, fmt.Errorf("actions/checkout fetch-depth %q is not a non-negative integer (0 = full history)", v)
				}
			}
			if strings.TrimLeft(v, "0") == "" {
				opts.Depth = "" // "00…" is 0: full history
			} else {
				opts.Depth = v
			}
		}
	}
	if v, ok := with["fetch-tags"]; ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "", "false", "0", "no":
			opts.Tags = false
		case "true", "1", "yes":
			opts.Tags = true
		default:
			return checkoutOpts{}, fmt.Errorf("actions/checkout fetch-tags %q is not a boolean (true / false)", v)
		}
	}
	if v, ok := with["ref"]; ok {
		ref := strings.TrimSpace(v)
		if ref == "" {
			return checkoutOpts{}, fmt.Errorf("actions/checkout ref is empty: name a branch, tag or commit SHA")
		}
		if len(ref) > 256 || strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, " \t\r\n\x00") {
			return checkoutOpts{}, fmt.Errorf("actions/checkout ref %q is not a valid ref (no whitespace, no leading '-', at most 256 bytes)", ref)
		}
		opts.Ref = ref
	}
	if v, ok := with["path"]; ok {
		rel := strings.TrimSpace(v)
		if rel == "" {
			// An empty path is the workspace root, exactly like the default.
			return opts, nil
		}
		if err := checkCheckoutPath(rel); err != nil {
			return checkoutOpts{}, err
		}
		opts.Path = rel
	}
	return opts, nil
}

// checkCheckoutPath validates a checkout `path`: a relative directory inside the
// workspace, never an absolute path, a home reference or an escaping `..`.
func checkCheckoutPath(rel string) error {
	if len(rel) > 512 || strings.ContainsAny(rel, "\x00") {
		return fmt.Errorf("actions/checkout path %q is not a valid directory (at most 512 bytes)", rel)
	}
	if strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "~") {
		return fmt.Errorf("actions/checkout path %q must be relative to the workspace (%s), not absolute or a home reference", rel, DefaultWorkspace)
	}
	for _, seg := range strings.Split(strings.ReplaceAll(rel, "\\", "/"), "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			return fmt.Errorf("actions/checkout path %q must not contain '..'", rel)
		}
	}
	return nil
}

// checkoutScript builds the in-container clone (15 §4): git, the job token as
// an askpass-style credential helper (never written into .git/config), the exact
// run commit (or the requested ref), shallow by default, into the requested
// workspace subdirectory.
//
// The argv is explicit and pinned by docs/threads.md §4.4:
//
//	git init -q
//	git remote add origin <url>
//	git -c credential.helper=<helper> fetch -q [--depth=N] --no-tags|--tags origin <sha|ref>
//	git checkout -q FETCH_HEAD
//
// A failed fetch does NOT end at git's bare `exit 128`: the script prints git's
// own output verbatim (the evidence), then a named diagnostic that states the
// URL tried, the reachability requirement and what to configure. The first-run
// trap this closes is an unreachable checkout origin (a host-only name such as
// walgit.localhost, or a proxy address), which is otherwise indistinguishable
// from any other git failure in the log.
func checkoutScript(p *Plan, opts checkoutOpts) string {
	if p.CheckoutURL == "" {
		return "echo 'spindle: no checkout URL in the job payload' >&2; exit 1"
	}
	dir := containerDir(p.Workspace, opts.Path)
	target := p.CheckoutSHA
	if opts.Ref != "" {
		target = opts.Ref
	}
	depthFlag := ""
	if opts.Depth != "" {
		depthFlag = "--depth=" + opts.Depth + " "
	}
	tagsFlag := "--no-tags "
	if opts.Tags {
		tagsFlag = "--tags "
	}
	// The helper's $SPINDLE_JOB_TOKEN is expanded by git's own shell, not ours:
	// it stays inside single quotes here.
	helper := `!f() { echo username=ci; echo password=$SPINDLE_JOB_TOKEN; }; f`
	fetch := "git -c credential.helper=" + shQuote(helper) + " fetch -q " + depthFlag + tagsFlag + "origin " + shQuote(target)
	lines := []string{"set -e"}
	if dir != p.Workspace {
		lines = append(lines, "mkdir -p "+shQuote(dir))
	}
	lines = append(lines,
		"cd "+shQuote(dir),
		"git init -q",
		"git remote remove origin >/dev/null 2>&1 || true",
		"git remote add origin "+shQuote(p.CheckoutURL),
		"if checkout_out=$("+fetch+" 2>&1); then",
		"\t:",
		"else",
		"\trc=$?",
		"\tprintf '%s\\n' \"$checkout_out\" >&2",
		"\tcase \"$checkout_out\" in",
		"\t*\"Could not resolve\"*|*\"unable to access\"*|*\"Connection refused\"*|*\"Failed to connect\"*|*\"Network is unreachable\"*|*\"No route to host\"*|*\"Temporary failure in name resolution\"*|*\"timed out\"*)",
	)
	lines = append(lines, printfLines(p.checkoutHint(true))...)
	lines = append(lines, "\t\t;;")
	lines = append(lines, "\t*)")
	lines = append(lines, printfLines(p.checkoutHint(false))...)
	lines = append(lines, "\t\t;;", "\tesac", "\texit \"$rc\"", "fi",
		"git checkout -q FETCH_HEAD",
	)
	done := "checked out " + p.CheckoutSHA
	if opts.Ref != "" {
		done += " (ref " + opts.Ref + ")"
	}
	lines = append(lines, "echo "+shQuote(done))
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
// token (under all its aliases), the OIDC request token, and any variable whose
// `env:` value was a whole secret reference. A docker action step never carries
// any of them (its EnvFrom/EnvSecret are empty), because the image is
// third-party code.
func credentialEnv(p *Plan, from []string, secretEnv map[string]string) []string {
	if p == nil {
		return nil
	}
	var out []string
	for _, name := range jobTokenEnvNames() {
		if containsString(from, name) {
			out = append(out, name+"="+p.JobToken)
		}
	}
	if p.IDTokenRequestToken != "" {
		if containsString(from, "SPINDLE_ID_TOKEN_REQUEST_TOKEN") {
			out = append(out, "SPINDLE_ID_TOKEN_REQUEST_TOKEN="+p.IDTokenRequestToken)
		}
		if containsString(from, "ACTIONS_ID_TOKEN_REQUEST_TOKEN") {
			out = append(out, "ACTIONS_ID_TOKEN_REQUEST_TOKEN="+p.IDTokenRequestToken)
		}
	}
	targets := make([]string, 0, len(secretEnv))
	for name := range secretEnv {
		targets = append(targets, name)
	}
	sort.Strings(targets)
	for _, name := range targets {
		// A missing secret (untrusted run, wrong name) is an EMPTY value, exactly
		// like GitHub's empty secrets context: the variable exists, the test
		// `[ -n "$X" ]` is false.
		value, _ := p.secretValue(secretEnv[name])
		out = append(out, name+"="+value)
	}
	return out
}

// execArgv builds `docker exec` for one step: the step environment first, then
// the NAME-only credentials (their values come from credentialEnv). Secret
// values are appended as `--env NAME` (the value is supplied through the docker
// client's own environment, so it never appears in argv) and masked in the log
// stream.
func execArgv(p *Plan, workdir string, env map[string]string, from []string, secretEnv map[string]string, cmd []string, withSecrets bool) []string {
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
	if withSecrets {
		secretNames := make([]string, 0, len(p.Secrets))
		for name := range p.Secrets {
			secretNames = append(secretNames, name)
		}
		sort.Strings(secretNames)
		for _, name := range secretNames {
			argv = append(argv, "--env", name)
		}
	}
	// Bare `--env NAME`: the value comes from the docker CLI's own environment
	// (the executor sets it from the plan's token), so it never appears in argv
	// of the exec call, the container's create argv or .git/config.
	for _, name := range from {
		argv = append(argv, "--env", name)
	}
	// An `env: NAME: ${{ secrets.X }}` mapping is the same trick under the
	// variable's own name: bare --env NAME plus the value in the docker client's
	// environment (credentialEnv), never in argv.
	targets := make([]string, 0, len(secretEnv))
	for name := range secretEnv {
		targets = append(targets, name)
	}
	sort.Strings(targets)
	for _, name := range targets {
		argv = append(argv, "--env", name)
	}
	argv = append(argv, p.Names.Container)
	argv = append(argv, cmd...)
	return argv
}

// The `shell:` vocabulary (docs/WORKFLOWS.md §4.1, PAR-079).
//
// A named shell maps onto the platform's argv with the documented defaults:
// `sh -e`, `bash -eo pipefail`, `python`/`python3`, `pwsh`/`powershell`, `cmd`.
// Anything else is an arbitrary command template that MUST contain exactly one
// `{0}` placeholder (GitHub's documented custom-shell form): the runner writes
// the step's script to a file inside the job container and substitutes its
// path. A template without `{0}` is a named step error — never a guess about
// where the script should go, and never "append the script to argv".
//
// The script file exists only for the custom form: a named shell keeps the
// script on the interpreter's command line (`-c`), which is the argv the
// runner has always produced and needs no extra container round trip.
const (
	// shellScriptPrefix is the per-step in-container script path. The flat
	// step index is unique across composite expansion (like the output file),
	// so two steps of one job can never read each other's script.
	shellScriptPrefix = "/tmp/spindle-step-"
)

// stepScriptPath is the in-container path of one step's script file.
func stepScriptPath(index int) string {
	return shellScriptPrefix + strconv.Itoa(index+1)
}

// shellCommand maps a workflow `shell:` onto argv. The returned bool reports
// whether the script must be written to scriptPath first (the custom form);
// a named shell carries the script inline and never touches the file.
func shellCommand(shell, script, scriptPath string) ([]string, bool, error) {
	trimmed := strings.TrimSpace(shell)
	name := trimmed
	if i := strings.IndexByte(name, ' '); i >= 0 {
		name = name[:i]
	}
	// A named shell accepts the alias alone or with its one `{0}` placeholder
	// (`bash`, `bash {0}`); anything else is the arbitrary command form below,
	// so `sh {0} {0}` is refused instead of silently dropping an argument.
	if extra := strings.TrimSpace(trimmed[len(name):]); extra == "" || extra == "{0}" {
		switch strings.ToLower(name) {
		case "", "sh":
			return []string{"sh", "-e", "-c", script}, false, nil
		case "bash":
			return []string{"bash", "--noprofile", "--norc", "-eo", "pipefail", "-c", script}, false, nil
		case "python":
			return []string{"python", "-c", script}, false, nil
		case "python3":
			return []string{"python3", "-c", script}, false, nil
		case "pwsh", "powershell":
			return []string{"pwsh", "-command", script}, false, nil
		case "cmd":
			return []string{"cmd", "/D", "/E:ON", "/V:OFF", "/S", "/C", script}, false, nil
		}
	}
	// The arbitrary form: split the template into words (no shell expansion —
	// the template is the workflow's own argv, never a string handed to a
	// shell), and require exactly one `{0}` after a real command word.
	words := threads.SplitOptions(trimmed)
	if len(words) == 0 || words[0] == "" || words[0] == "{0}" {
		return nil, false, fmt.Errorf("unsupported shell %q: a custom shell needs a command that reads the script, like `dash -e {0}` (named shells: sh, bash, python, python3, pwsh, powershell, cmd)", shell)
	}
	placed := 0
	argv := make([]string, 0, len(words))
	for _, word := range words {
		for strings.Contains(word, "{0}") {
			i := strings.Index(word, "{0}")
			word = word[:i] + scriptPath + word[i+len("{0}"):]
			placed++
		}
		argv = append(argv, word)
	}
	if placed != 1 {
		return nil, false, fmt.Errorf("unsupported shell %q: a custom shell command must contain exactly one {0} placeholder for the script file (named shells: sh, bash, python, python3, pwsh, powershell, cmd)", shell)
	}
	if len(argv) == 0 || argv[0] == "" {
		return nil, false, fmt.Errorf("unsupported shell %q: names no command", shell)
	}
	return argv, true, nil
}

// replaceStepCommand swaps the command tail of one docker exec argv, keeping
// its environment prefix. It is the one place a step's argv is rebuilt after
// planning (the bash→sh fallback), so the fallback cannot drop an env layer.
func replaceStepCommand(step StepPlan, container string, cmd []string) StepPlan {
	idx := -1
	for i, arg := range step.Argv {
		if arg == container {
			idx = i
			break
		}
	}
	if idx < 0 {
		return step // an argv shape this helper does not own: never mangle it
	}
	argv := make([]string, 0, idx+1+len(cmd))
	argv = append(argv, step.Argv[:idx+1]...)
	argv = append(argv, cmd...)
	step.Argv = argv
	return step
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

// checkWorkflowContainer applies the container option/mount policy at plan time
// with the runner's own grants (defence in depth: the job must not start even
// if it was created by an older or bypassed server). field is the record prefix
// the diagnostic names ("container" or "services.<id>").
func checkWorkflowContainer(pol threads.ContainerPolicy, field, options string, volumes []string) error {
	if r := threads.CheckWorkflowContainerOptionsWith(pol, field, options, volumes); r != nil {
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
