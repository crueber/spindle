package threads

// The server-side image gate (docs/PLAN-ARTIFACT-KEEPER.md §3.3, layer L2):
// threads.image_allow_prefixes and threads.image_require_digest, enforced where
// a workflow becomes a job (createOne), for EVERY trust class — a same-repo
// pull request runs a workflow file from the PR head with the repository's
// secrets in the job environment, so it can name any image it likes unless the
// server says no. A fork run takes the same check (it merely never receives the
// secrets that made the hole interesting).
//
// The refusal is a job-level failure with a Reason (the field the page and the
// API already render) and a red check mirror; nothing about it reaches the
// runner, and the allowlist is never shipped in the job spec (L3 is rejected in
// the plan: a runner cannot fix a server that hands it a hostile payload).
//
// Deliberately NOT here: the runner's own --image. That is the operator's
// declaration (pin it by digest in the unit or container definition), not
// workflow-controlled input.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/crueber/spindle/internal/threads/workflow"
)

// ImagePolicy is the instance-wide image gate the service was configured with.
type ImagePolicy struct {
	// AllowPrefixes are literal host[:port]/path prefixes; empty = no
	// restriction (today's behaviour, the compatible default).
	AllowPrefixes []string
	// RequireDigest refuses an image without @sha256:<64 hex>.
	RequireDigest bool
}

// ImageRefusal is one refused image: the reference, the field it came from
// ("container.image" or "services.<id>.image") and the operator-facing reason.
// It carries no secret material by construction — only the reference and the
// configured prefixes.
type ImageRefusal struct {
	Image  string
	Source string
	Reason string
}

// String renders the reason stored on the job record.
func (r ImageRefusal) String() string { return r.Reason }

// imagePolicy returns the service's configured policy.
func (s *Service) imagePolicy() ImagePolicy {
	return ImagePolicy{AllowPrefixes: s.ImageAllowPrefixes, RequireDigest: s.ImageRequireDigest}
}

// imageRefusalReason returns the job-level Reason when the service's image
// policy refuses this spec, and whether it refused. An unevaluable policy is a
// refusal (fail closed). It is the single gate both run creation and rerun
// use, so a policy that was tightened after a run was created also stops its
// reruns.
func (s *Service) imageRefusalReason(spec *Spec) (string, bool) {
	if refusal, err := s.imagePolicy().checkJobImages(spec); err != nil {
		return fmt.Sprintf("image policy could not be evaluated (%v); refusing to run the job (fail closed)", err), true
	} else if refusal != nil {
		return refusal.Reason, true
	}
	return "", false
}

// checkJobImages applies the policy to a job spec's container and every
// service image. A nil refusal means the job may run. An unevaluable policy
// (an invalid allowlist entry reaching the service) is itself an error, and
// the caller refuses the job: the gate fails closed, never open.
//
// It is also the ONE gate for action images (docs/threads.md §3.4a): a
// `uses: docker://image` step, a local docker action's image, and any
// docker:// step inside a composite action all pass through checkImage with a
// source naming the step (and the nested chain) they came from. There is no
// second allowlist: an action image is workflow-named code exactly like a job
// container, so the same prefixes and the same digest rule apply.
func (p ImagePolicy) checkJobImages(spec *Spec) (*ImageRefusal, error) {
	if spec == nil {
		return nil, nil
	}
	if spec.Container != nil {
		if refusal, err := p.checkImage(spec.Container.Image, "container.image"); err != nil || refusal != nil {
			return refusal, err
		}
	}
	// Sorted ids: a job with two forbidden services must refuse the same one
	// on every evaluation (the record is the evidence).
	ids := make([]string, 0, len(spec.Services))
	for id := range spec.Services {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		source := "services." + id + ".image"
		if refusal, err := p.checkImage(spec.Services[id].Image, source); err != nil || refusal != nil {
			return refusal, err
		}
	}
	if refusal, err := p.checkStepImages(spec, spec.Steps, "", nil, 1); err != nil || refusal != nil {
		return refusal, err
	}
	return nil, nil
}

// checkStepImages walks a step list for docker images: a docker:// step, a
// local docker action, or a docker:// step inside a composite action. The walk
// is bounded (depth and a visiting set) so a hostile stored spec cannot loop
// it; a local action the resolver did not embed has no image to check here —
// creation already refused that job before the gate ran.
func (p ImagePolicy) checkStepImages(spec *Spec, steps []StepSpec, parent string, visiting []string, depth int) (*ImageRefusal, error) {
	for i, st := range steps {
		source := fmt.Sprintf("step %d", i+1)
		if parent != "" {
			source = fmt.Sprintf("%s step %d", parent, i+1)
		}
		uses := strings.TrimSpace(st.Uses)
		if image := DockerUsesImage(uses); image != "" {
			if refusal, err := p.checkImage(image, source+" (uses: "+uses+")"); err != nil || refusal != nil {
				return refusal, err
			}
			continue
		}
		if !IsLocalAction(uses) {
			continue
		}
		dir, derr := LocalActionDir(uses)
		if derr != nil || indexOf(visiting, dir) >= 0 || depth > maxActionWalkDepth {
			continue // a malformed/looped definition is refused by the resolver
		}
		def, ok := spec.Actions[dir]
		if !ok {
			continue
		}
		switch def.Using {
		case workflow.ActionUsingDocker:
			if refusal, err := p.checkImage(def.Image, source+" (uses: "+uses+")"); err != nil || refusal != nil {
				return refusal, err
			}
		case workflow.ActionUsingComposite:
			if refusal, err := p.checkStepImages(spec, def.Steps, source+" (uses: "+uses+")",
				append(visiting, dir), depth+1); err != nil || refusal != nil {
				return refusal, err
			}
		}
	}
	return nil, nil
}

// maxActionWalkDepth bounds a defensive display/gate walk (one over the
// resolver's limit, so a stored spec from an older instance still terminates).
const maxActionWalkDepth = 12

// checkImage decides one reference. The allowlist is checked before the
// digest rule, so "not on the allowlist" is the reason when both apply (the
// more specific configuration wins the message).
//
// A host-less reference is a Docker Hub reference under docker's own grammar,
// so it is matched BOTH literally ("golang" allows golang:1.27 if the operator
// chose to list it) and in its canonical spelling ("docker.io/library/golang",
// which is what a "docker.io" or "docker.io/library" entry names). That is the
// same normalisation docker applies when it resolves the pull: not an alias,
// the same reference. An entry never matches across registries — the canonical
// form only ever names docker.io.
func (p ImagePolicy) checkImage(image, source string) (*ImageRefusal, error) {
	image = strings.TrimSpace(image)
	if image == "" {
		return nil, nil // no image named: nothing to gate (the runner's default applies)
	}
	if len(p.AllowPrefixes) > 0 {
		candidates := imageMatchForms(image)
		allowed := false
		for _, raw := range p.AllowPrefixes {
			prefix, err := normalizeImagePrefix(raw)
			if err != nil {
				return nil, fmt.Errorf("threads.image_allow_prefixes entry %q is invalid: %v", raw, err)
			}
			for _, candidate := range candidates {
				if imageHasPrefix(candidate, prefix) {
					allowed = true
					break
				}
			}
			if allowed {
				break
			}
		}
		if !allowed {
			return &ImageRefusal{
				Image: image, Source: source,
				Reason: fmt.Sprintf("image %q (%s) is not allowed by threads.image_allow_prefixes (allowed: %s)", image, source, strings.Join(p.AllowPrefixes, ", ")),
			}, nil
		}
	}
	if p.RequireDigest && !hasImageDigest(image) {
		return &ImageRefusal{
			Image: image, Source: source,
			Reason: fmt.Sprintf("image %q (%s) is not digest-pinned: threads.image_require_digest requires @sha256:<64 hex>. Pin it with `docker buildx imagetools inspect %s` (or `docker image inspect --format '{{index .RepoDigests 0}}' %s` after a pull)", image, source, image, image),
		}, nil
	}
	return nil, nil
}

// imageHasPrefix reports whether image starts with prefix at a reference
// boundary. The boundary set is '/', ':', '@' or the end of the string: a
// prefix names a repository PATH (docker.io/library allows
// docker.io/library/golang:1.27), a repository (ghcr.io/acme), or — because
// ':' is a boundary — a host and port (localhost:5000 allows every repository
// on it).
func imageHasPrefix(image, prefix string) bool {
	if prefix == "" || !strings.HasPrefix(image, prefix) {
		return false
	}
	if len(image) == len(prefix) {
		return true
	}
	switch image[len(prefix)] {
	case '/', ':', '@':
		return true
	}
	return false
}

// imageMatchForms is the set of spellings one reference is matched under: the
// authored reference, plus its canonical Docker Hub form when it names no
// registry (docker's own implicit "docker.io" and "library/" rules).
func imageMatchForms(image string) []string {
	forms := []string{image}
	if canonical := canonicalImageRef(image); canonical != image {
		forms = append(forms, canonical)
	}
	return forms
}

// canonicalImageRef expands a host-less reference to its Docker Hub spelling
// ("golang:1.27" → "docker.io/library/golang:1.27", "alpine/git" →
// "docker.io/alpine/git"). A reference that names a registry is returned
// unchanged, as is anything that is not a reference-shaped string.
func canonicalImageRef(image string) string {
	name, suffix := splitImageRef(image)
	first := name
	if i := strings.IndexByte(name, '/'); i >= 0 {
		first = name[:i]
	}
	if first == "" || strings.ContainsAny(first, ".:") || strings.EqualFold(first, "localhost") {
		return image
	}
	if strings.Contains(name, "/") {
		return "docker.io/" + name + suffix
	}
	return "docker.io/library/" + name + suffix
}

// splitImageRef separates a reference's name from its tag or digest, exactly
// as the runner's plan does (internal/threads/runner.splitImageRef): the tag
// must follow the last '/', and a digest starts at '@'.
func splitImageRef(ref string) (name, suffix string) {
	if i := strings.LastIndexByte(ref, '@'); i >= 0 {
		return ref[:i], ref[i:]
	}
	if i := strings.LastIndexByte(ref, ':'); i > strings.LastIndexByte(ref, '/') {
		return ref[:i], ref[i:]
	}
	return ref, ""
}

// imageAllowPrefixRe is the accepted spelling of one entry. It mirrors the
// config validator's rule (internal/config.validateImageAllowPrefix) and the
// runner's --image-prefix: the service re-checks rather than trusting its
// caller, because a policy that cannot be evaluated must refuse — a silently
// ignored bad entry would be an allow-everything bug.
var imageAllowPrefixRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*(?::[0-9]{1,5})?(?:/[a-z0-9][a-z0-9._-]*)+$`)

// normalizeImagePrefix validates one allowlist entry and returns it as the
// match key (trimmed; there is no case folding — image references are
// lowercase by docker's own grammar, and the validator rejects uppercase).
func normalizeImagePrefix(raw string) (string, error) {
	prefix := strings.TrimSpace(raw)
	switch {
	case prefix == "":
		return "", fmt.Errorf("must not be empty")
	case prefix != raw:
		return "", fmt.Errorf("must not have surrounding whitespace")
	case strings.Contains(prefix, "://"):
		return "", fmt.Errorf("must not carry a scheme (give host[:port]/path)")
	case strings.HasSuffix(prefix, "/"):
		return "", fmt.Errorf("must not end with '/'")
	case strings.Contains(prefix, "//") || strings.Contains(prefix, ".."):
		return "", fmt.Errorf("has an empty or '.' path segment")
	case !imageAllowPrefixRe.MatchString(prefix):
		return "", fmt.Errorf("must look like host[:port]/path in lowercase, e.g. docker.io/library")
	}
	return prefix, nil
}

// hasImageDigest reports whether image is pinned to a sha256 digest
// (@sha256:<64 hex>). The runner's --image-require-digest uses the same rule
// (internal/threads/runner.HasImageDigest); this package cannot import the
// runner (the runner imports this one).
func hasImageDigest(image string) bool {
	i := strings.LastIndexByte(image, '@')
	if i < 0 {
		return false
	}
	digest := image[i+1:]
	if !strings.HasPrefix(digest, "sha256:") {
		return false
	}
	hex := digest[len("sha256:"):]
	if len(hex) != 64 {
		return false
	}
	for _, c := range hex {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
