package runner

// Named image-pull diagnostics (docs/threads.md §8.6, the same shape as the
// checkout reachability diagnostic in §4.4). A `docker create` that fails
// inside the daemon used to reach the job log as docker's own text and
// nothing else, which leaves an operator unable to tell "the mirror is down",
// "the credential expired" and "the mirror repository key is gone" apart —
// three failures with three different fixes. The classifier reads docker's
// wording (the only signal the CLI boundary exposes), and every message
// states the reference actually attempted, the mirror prefix, whether a
// fallback to the public registry exists, and the fix.

import "strings"

// Image failure classes; "" means docker's text matched no known class (the
// caller then prints docker's error only, never a guessed diagnosis).
const (
	imageFailureUnreachable  = "unreachable"
	imageFailureUnauthorized = "unauthorized"
	imageFailureNotFound     = "not-found"
)

// unreachableMarkers are the daemon/DNS/TLS errors of a registry that did not
// answer. Checked first: "connection refused" inside a TLS handshake message
// must not be read as an auth failure.
var unreachableMarkers = []string{
	"connection refused",
	"no such host",
	"server misbehaving",
	"i/o timeout",
	"tls handshake timeout",
	"network is unreachable",
	"no route to host",
	"temporary failure in name resolution",
	"server gave http response to https client",
	"context deadline exceeded",
	"dial tcp",
	"connection reset",
	"unexpected eof",
	"timeout exceeded",
	"client.timeout",
}

// unauthorizedMarkers are a refused or missing credential. "pull access
// denied" is docker's ambiguous 401/404 text, so it lands here with a fix
// that names both possibilities.
var unauthorizedMarkers = []string{
	"unauthorized",
	"authentication required",
	"no basic auth credentials",
	"pull access denied",
	"denied: requested access",
	"401 ",
}

// notFoundMarkers are a repository or manifest the registry does not have.
var notFoundMarkers = []string{
	"manifest unknown",
	"name_unknown",
	"not found",
	"404",
	"does not exist",
}

// classifyImageFailure maps docker's failure text onto one class.
func classifyImageFailure(detail string) string {
	text := strings.ToLower(detail)
	for _, marker := range unreachableMarkers {
		if strings.Contains(text, marker) {
			return imageFailureUnreachable
		}
	}
	for _, marker := range unauthorizedMarkers {
		if strings.Contains(text, marker) {
			return imageFailureUnauthorized
		}
	}
	for _, marker := range notFoundMarkers {
		if strings.Contains(text, marker) {
			return imageFailureNotFound
		}
	}
	return ""
}

// imageHost is the registry host of a reference ("" for a host-less Docker
// Hub reference).
func imageHost(image string) string {
	name, _ := splitImageRef(image)
	return refRegistry(name)
}

// imageDiagnostic renders the named lines for one failed create, or nil when
// docker's error matches no class. image is the reference attempted and
// requested the reference before the prefix rewrite (they differ only when a
// mirror rewrote it).
func imageDiagnostic(p *Plan, image, requested, detail string) []string {
	kind := classifyImageFailure(detail)
	if kind == "" {
		return nil
	}
	host := imageHost(image)
	if host == "" {
		host = "the registry"
	}
	var headline string
	switch kind {
	case imageFailureUnreachable:
		headline = "image pull failed for " + image + " — " + host + " did not answer (a name, connection or TLS error; docker's text is above)"
	case imageFailureUnauthorized:
		headline = "image pull refused for " + image + " — " + host + " did not accept the credential (401), or has no such repository (the same 404 text)"
	default:
		headline = "image not found: " + image + " — " + host + " answered 404 (manifest unknown / NAME_UNKNOWN)"
	}
	lines := []string{"", "spindle: " + headline, "spindle:   " + imageMirrorLine(p, image, requested), "spindle:   pull: " + p.Images.pull() + imagePullNote(p.Images.pull())}
	switch kind {
	case imageFailureUnreachable:
		lines = append(lines,
			"spindle:   the pull happens in the DOCKER DAEMON, not in the runner, so the daemon must reach "+host+".",
			"spindle:   fix: check the daemon's DNS/network; a plain-HTTP registry must be listed in the daemon's",
			"spindle:   insecure-registries (and a TLS registry must present a certificate the daemon trusts).",
		)
		if p.Images.Prefix != "" {
			lines = append(lines, "spindle:   Or start the runner without --image-prefix to pull "+requested+" from its public registry.")
		}
	case imageFailureUnauthorized:
		lines = append(lines,
			"spindle:   fix: the daemon's docker config ($DOCKER_CONFIG or ~/.docker/config.json, mode 0600) needs a",
			"spindle:   pull-only token for "+host+" (write it with `docker login --password-stdin`, never in argv).",
			"spindle:   If the credential is fine, check the mirror repository named by the prefix — a missing",
			"spindle:   repository path produces this same message.",
		)
	default:
		lines = append(lines,
			"spindle:   fix: the mirror must have the repository the prefix names (one per upstream registry when",
			"spindle:   --image-prefix-all is set) with this image cached or proxyable; check the path and the tag/digest.",
		)
		if p.Images.Prefix != "" {
			lines = append(lines, "spindle:   Or start the runner without --image-prefix to pull "+requested+" from its public registry.")
		}
	}
	return lines
}

// imageMirrorLine states the image source and whether a fallback exists.
func imageMirrorLine(p *Plan, image, requested string) string {
	if p.Images.Prefix == "" {
		return "mirror: none (--image-prefix is off) — docker used the reference as written"
	}
	if image != requested {
		return "mirror: --image-prefix " + p.Images.Prefix + " rewrote " + requested + " to " + image + "; there is no fallback to the public registry (fail closed)"
	}
	return "mirror: --image-prefix " + p.Images.Prefix + " is set, but this reference names its own registry and was used as written (--image-prefix-all is off); there is no fallback"
}

// imagePullNote explains the pull policy's role in this failure.
func imagePullNote(pull string) string {
	switch pull {
	case PullAlways:
		return " — every job asks the registry, so a registry outage fails even a cached image"
	case PullNever:
		return " — only images already on the daemon run; nothing was pulled"
	default:
		return " — docker pulls only an absent image, so a tag already present is never refreshed"
	}
}
