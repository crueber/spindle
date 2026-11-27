package config

import (
	"fmt"
	"net"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/actions"
)

// Validate runs every §5 rule, plus the always-on authentication rule
// (PLAN-OIDC-ONLY §4.2): OIDC is the only authentication, so an incomplete
// OIDC set is a validation error. There is no warning path for auth any more.
func Validate(c *Config) (warnings []string, errs []error) {
	errs = append(errs, checkAuthConfigured(c)...)
	errs = append(errs, checkStaticTokens(c)...)
	errs = append(errs, checkListen(c)...)
	errs = append(errs, checkBundleStrategies(c)...)
	errs = append(errs, checkBundleChainFilters(c)...)
	errs = append(errs, checkStore(c)...)
	errs = append(errs, checkSizes(c)...)
	errs = append(errs, checkPlacementGlobs(c)...)
	errs = append(errs, checkRoles(c)...)
	errs = append(errs, checkPaths(c)...)
	errs = append(errs, checkSSH(c)...)
	errs = append(errs, checkImport(c)...)
	errs = append(errs, checkPackages(c)...)
	errs = append(errs, checkWebedit(c)...)
	errs = append(errs, checkStaticSites(c)...)
	errs = append(errs, checkTreeLog(c)...)
	errs = append(errs, checkSecretsKey(c)...)
	errs = append(errs, checkThreads(c)...)
	errs = append(errs, checkRateLimit(c)...)
	errs = append(errs, checkSearch(c)...)
	errs = append(errs, checkEventSinks(c)...)
	return warnings, errs
}

// checkSearch validates the [search] section (docs/features/20_search.md
// §5.2/§11). A zero max_concurrent means "the default" (2); a negative or
// above-ceiling value is a configuration mistake that would otherwise be
// silently clamped somewhere the operator cannot see. enabled is a *bool:
// absent/nil is enabled, so there is nothing to validate there.
func checkSearch(c *Config) []error {
	n := c.Search.MaxConcurrent
	switch {
	case n < 0:
		return []error{fmt.Errorf("search.max_concurrent must not be negative (0 = the default of 2; ceiling %d)", SearchMaxConcurrent)}
	case n > SearchMaxConcurrent:
		return []error{fmt.Errorf("search.max_concurrent is %d; the ceiling is %d (each search can hold four grep workers and a scan-pool slot, and clone/upload-pack must keep its CPU)", n, SearchMaxConcurrent)}
	}
	return nil
}

// checkRateLimit validates the [rate_limit] rate spellings (PLAN-V2 §M.7).
// Two independent spellings can carry the same mistake; each is reported with
// its own key so the settings page marks the exact row.
func checkRateLimit(c *Config) []error {
	if _, err := c.RateLimit.ParseRates(); err != nil {
		return []error{err}
	}
	return nil
}

// checkEventSinks validates every [[events.sinks]] entry (14 §14.6) with errors
// that name the exact key, so the setup page can mark the row: kind is one of
// the documented built-ins, name (when set) is unique and URL/path safe, the
// kind's required keys are present, and the jsonl rotation knobs are sane.
func checkEventSinks(c *Config) []error {
	var errs []error
	seen := map[string]bool{}
	for i, s := range c.Events.Sinks {
		at := fmt.Sprintf("events.sinks[%d]", i)
		kind := strings.TrimSpace(s.Kind)
		name := strings.TrimSpace(s.Name)
		if name != "" {
			if seen[name] {
				errs = append(errs, fmt.Errorf("%s.name %q is used twice", at, name))
			}
			seen[name] = true
		}
		switch kind {
		case EventSinkWebhook:
			if strings.TrimSpace(s.URL) == "" {
				errs = append(errs, fmt.Errorf("%s.url is required for kind %q", at, EventSinkWebhook))
			}
			if strings.TrimSpace(s.Path) != "" {
				errs = append(errs, fmt.Errorf("%s.path is a jsonl key (kind %q ignores it); remove it or switch kind", at, EventSinkWebhook))
			}
		case EventSinkJSONL:
			if strings.TrimSpace(s.Path) == "" {
				errs = append(errs, fmt.Errorf("%s.path is required for kind %q (the NDJSON file to append)", at, EventSinkJSONL))
			}
			if strings.TrimSpace(s.URL) != "" {
				errs = append(errs, fmt.Errorf("%s.url is a webhook key (kind %q ignores it); remove it or switch kind", at, EventSinkJSONL))
			}
			if s.MaxBytes < 0 {
				errs = append(errs, fmt.Errorf("%s.max_bytes must be positive (default %d)", at, DefaultEventSinkMaxBytes))
			}
			if s.Keep < 0 {
				errs = append(errs, fmt.Errorf("%s.keep must be >= 1 (default %d)", at, DefaultEventSinkKeep))
			}
		default:
			errs = append(errs, fmt.Errorf("%s.kind must be %q or %q (got %q)", at, EventSinkWebhook, EventSinkJSONL, s.Kind))
		}
	}
	return errs
}

// checkThreads (15_threads.md §8): the retention window must be a real window
// (a negative value would delete everything the pass can see) and the default
// job permission must be one of the two levels a token can be scoped to.
//
// PAR-179 bounds, with the same numbers the Threads retention pass enforces:
// the run/log and artifact windows are 1 day…1 year (a 90-day setting is the
// documented parity case; keep_forever is the unbounded spelling), a requeue
// ceiling is 1…20, and the base job lease is 5m…30m (the effective lease is
// capped at 30m because log frames renew it).
const (
	threadsMinRetention     = 24 * time.Hour
	threadsMaxRetention     = 365 * 24 * time.Hour
	threadsMaxQueueAttempts = 20
	threadsMinJobLease      = 5 * time.Minute
	threadsMaxJobLease      = 30 * time.Minute
)

func checkThreads(c *Config) []error {
	var errs []error
	if time.Duration(c.Threads.Retention) < 0 {
		errs = append(errs, fmt.Errorf("threads.retention must not be negative (got %s); leave it unset for the 30d default", time.Duration(c.Threads.Retention)))
	} else if r := time.Duration(c.Threads.Retention); r > 0 && (r < threadsMinRetention || r > threadsMaxRetention) {
		errs = append(errs, fmt.Errorf("threads.retention must be between %s and %s (got %s); use threads.keep_forever for an unbounded window",
			threadsMinRetention, threadsMaxRetention, r))
	}
	if r := time.Duration(c.Threads.ArtifactRetention); r != 0 && (r < threadsMinRetention || r > threadsMaxRetention) {
		errs = append(errs, fmt.Errorf("threads.artifact_retention must be between %s and %s (got %s), or unset to follow threads.retention",
			threadsMinRetention, threadsMaxRetention, r))
	}
	if n := c.Threads.MaxQueueAttempts; n < 0 || n > threadsMaxQueueAttempts {
		errs = append(errs, fmt.Errorf("threads.max_queue_attempts must be 1…%d (got %d)", threadsMaxQueueAttempts, n))
	}
	if l := time.Duration(c.Threads.JobLease); l != 0 && (l < threadsMinJobLease || l > threadsMaxJobLease) {
		errs = append(errs, fmt.Errorf("threads.job_lease must be between %s and %s (got %s)",
			threadsMinJobLease, threadsMaxJobLease, l))
	}
	switch strings.ToLower(strings.TrimSpace(c.Threads.DefaultPermissions)) {
	case "", "read", "write":
	default:
		errs = append(errs, fmt.Errorf("threads.default_permissions must be read or write (got %q)", c.Threads.DefaultPermissions))
	}
	// Artifact caps: a negative cap is meaningless; a zero selects the default.
	if c.Threads.MaxArtifactBytesPerRun < 0 {
		errs = append(errs, fmt.Errorf("threads.max_artifact_bytes_per_run must not be negative (got %d)", c.Threads.MaxArtifactBytesPerRun))
	}
	if c.Threads.MaxArtifactBytesPerFile < 0 {
		errs = append(errs, fmt.Errorf("threads.max_artifact_bytes_per_file must not be negative (got %d)", c.Threads.MaxArtifactBytesPerFile))
	}
	// Cache service bounds (PAR-095). A zero selects the documented default; a
	// negative value is meaningless and refused by name.
	if c.Threads.CacheMaxBytes < 0 {
		errs = append(errs, fmt.Errorf("threads.cache_max_bytes must not be negative (got %d)", c.Threads.CacheMaxBytes))
	}
	if c.Threads.CacheMaxEntryBytes < 0 {
		errs = append(errs, fmt.Errorf("threads.cache_max_entry_bytes must not be negative (got %d)", c.Threads.CacheMaxEntryBytes))
	}
	if r := time.Duration(c.Threads.CacheRetention); r < 0 {
		errs = append(errs, fmt.Errorf("threads.cache_retention must not be negative (got %s)", r))
	}
	if n := c.Threads.CacheMaxEntries; n < 0 {
		errs = append(errs, fmt.Errorf("threads.cache_max_entries must not be negative (got %d)", n))
	}
	// Image allowlist (PLAN-ARTIFACT-KEEPER §5.2): each entry is a literal
	// host[:port]/path prefix. An error names the key AND the row so the
	// settings page can mark it; a scheme or a trailing slash is the classic
	// paste of a URL, and it must fail loudly rather than match nothing and
	// lock every job out (or, worse, be silently ignored).
	for i, prefix := range c.Threads.ImageAllowPrefixes {
		if err := validateImageAllowPrefix(prefix); err != nil {
			errs = append(errs, fmt.Errorf("threads.image_allow_prefixes[%d]: %v", i, err))
		}
	}
	// Remote-action policy (PAR-070/D-5). The policy package owns the rules so
	// the resolver and the config loader cannot disagree about what an entry
	// means.
	policy := actions.Policy{
		Mode: actions.Mode(c.Threads.Actions.Mode), Allow: c.Threads.Actions.Allow,
		RequireSHA: c.Threads.Actions.RequireSHA,
	}
	if err := policy.Validate(); err != nil {
		errs = append(errs, err)
	}
	if src := strings.TrimSpace(c.Threads.Actions.Source); src != "" {
		if _, err := actions.NewHTTPSource(src, actions.DefaultLimits()); err != nil {
			errs = append(errs, err)
		}
	}
	if c.Threads.Actions.MaxBytes < 0 {
		errs = append(errs, fmt.Errorf("threads.actions.max_bytes must not be negative (got %d)", c.Threads.Actions.MaxBytes))
	}
	return errs
}

// imageAllowPrefixRe is the accepted spelling of one image allowlist entry
// (and of the runner's --image-prefix, which the mirrors doc keeps identical):
// a registry host (hostname or IPv4, optional port) plus at least one
// lowercase path segment. No scheme, no tag, no digest, no trailing slash.
var imageAllowPrefixRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*(?::[0-9]{1,5})?(?:/[a-z0-9][a-z0-9._-]*)+$`)

// validateImageAllowPrefix validates one threads.image_allow_prefixes entry.
func validateImageAllowPrefix(prefix string) error {
	switch {
	case prefix == "":
		return fmt.Errorf("must not be empty (remove the entry, or name a registry path such as docker.io/library)")
	case strings.TrimSpace(prefix) != prefix:
		return fmt.Errorf("%q has surrounding whitespace", prefix)
	case strings.Contains(prefix, "://"):
		return fmt.Errorf("%q must not carry a scheme: give host[:port]/path, e.g. docker.io/library or ak.example/dockerhub", prefix)
	case strings.HasSuffix(prefix, "/"):
		return fmt.Errorf("%q must not end with '/': give host[:port]/path, e.g. docker.io/library", prefix)
	case strings.Contains(prefix, "@"):
		return fmt.Errorf("%q must name a repository path, not a digest (prefixes match at /, :, @ or end)", prefix)
	case strings.Contains(prefix, "//") || strings.Contains(prefix, ".."):
		return fmt.Errorf("%q has an empty or '.' path segment", prefix)
	case hasImageTagInPath(prefix):
		return fmt.Errorf("%q must not name a tag: give the repository prefix only (docker.io/library, not docker.io/library:latest) — prefixes match at /, :, @ or end", prefix)
	case !imageAllowPrefixRe.MatchString(prefix):
		return fmt.Errorf("%q must look like host[:port]/path in lowercase (hostname or IPv4, e.g. docker.io/library, ghcr.io/acme, localhost:5000/ci)", prefix)
	}
	return nil
}

// hasImageTagInPath reports a ':' after the first '/', i.e. a tag rather than
// the host's port.
func hasImageTagInPath(prefix string) bool {
	i := strings.IndexByte(prefix, '/')
	return i >= 0 && strings.Contains(prefix[i:], ":")
}

// checkListen validates the HTTP listener shape before boot: a malformed
// server.listen otherwise fails only inside net.Listen, where setup-only mode
// could not explain it.
func checkListen(c *Config) []error {
	if strings.TrimSpace(c.Server.Listen) == "" {
		return []error{fmt.Errorf("server.listen is required (host:port, e.g. 0.0.0.0:8080)")}
	}
	if _, _, err := net.SplitHostPort(c.Server.Listen); err != nil {
		return []error{fmt.Errorf("server.listen %q must be host:port (e.g. 0.0.0.0:8080)", c.Server.Listen)}
	}
	return nil
}

// checkSecretsKey (15_threads.md §5.3): the dedicated Threads key is optional,
// but a short one is a configuration error (the derived AES-256/HMAC keys need a
// real secret, not a passphrase placeholder).
func checkSecretsKey(c *Config) []error {
	if k := c.Server.SecretsKey; k != "" && len(k) < 32 {
		return []error{fmt.Errorf("server.secrets_key must be at least 32 bytes (got %d)", len(k))}
	}
	return nil
}

// checkSSH (17_ssh.md §3): the transport is disabled unless listen is set;
// user keys are not config (they live in the object store, managed through
// the UI/API), so only the listener shape is validated here. external_port
// is the advertised port for ssh:// clone URLs (0 = the listen port); when
// set it must be a TCP port.
func checkSSH(c *Config) []error {
	sc := c.Server.SSH
	if sc.Listen == "" && sc.HostKey == "" && sc.HostKeyEnv == "" && sc.ExternalPort == 0 {
		return nil
	}
	var errs []error
	if sc.Listen != "" {
		if _, _, err := net.SplitHostPort(sc.Listen); err != nil {
			errs = append(errs, fmt.Errorf("server.ssh.listen %q must be host:port", sc.Listen))
		}
	}
	if sc.ExternalPort < 0 || sc.ExternalPort > 65535 {
		errs = append(errs, fmt.Errorf("server.ssh.external_port %d must be a TCP port 1-65535 (0 = the listen port)", sc.ExternalPort))
	}
	return errs
}

// checkPackages (docs/features/14 §5): the OCI registry limits must be real
// bounds. A non-positive size or TTL is an error (there is no "unlimited
// blob" — a 0 would read as the compiled-in default inside the package, and a
// silently substituted default at boot is exactly the sort of drift the setup
// page would then show wrong).
func checkPackages(c *Config) []error {
	p := c.Packages
	var errs []error
	if p.MaxBlobBytes <= 0 {
		errs = append(errs, fmt.Errorf("packages.max_blob_bytes must be positive (default 10GiB)"))
	}
	if p.MaxManifestBytes <= 0 {
		errs = append(errs, fmt.Errorf("packages.max_manifest_bytes must be positive (default 4MiB)"))
	}
	if p.UploadTTL <= 0 {
		errs = append(errs, fmt.Errorf("packages.upload_ttl must be positive (default 24h)"))
	}
	if p.QuotaBytes < 0 {
		errs = append(errs, fmt.Errorf("packages.quota_bytes must be zero (unlimited) or positive"))
	}
	return errs
}

// checkWebedit (docs/features/17_file_edit.md): the editor cap must be a
// real bound. A zero would read as the compiled default inside the package,
// so a silently substituted default at boot is drift the setup page would
// then show wrong.
func checkWebedit(c *Config) []error {
	if c.Webedit.MaxBytes <= 0 {
		return []error{fmt.Errorf("webedit.max_bytes must be positive (default 1MiB)")}
	}
	return nil
}

// checkStaticSites validates the additive [static_sites] section
// (docs/features/19 §5), fail closed. With enabled = false the listener is
// never constructed, so nothing else in the section can hurt — a disabled
// section is accepted as written (an operator may stage a config before
// turning it on). With enabled = true every rule below holds:
//
//   - host is required: it is the ONLY Host the listener answers for, and an
//     empty one would refuse every request (or, worse, invite a wildcard
//     interpretation in the handler);
//   - host is not the application's own hostname: the whole point of the
//     second listener is a second origin (§2). If static_sites.host equals the
//     app's host, a published site's HTML/JS runs on the session origin and can
//     read the visitor's cookies — the account-takeover case the feature
//     forbids. The comparison is case-insensitive and port-insensitive, and
//     reads server.public_url's hostname when it is set, else server.listen's
//     (only when that names a real host, never the wildcard 0.0.0.0/::);
//   - address parses as host:port and is not the app's own listener. The
//     comparison is an exact string comparison of the two configured values —
//     the repo has no address-normalisation helper, and a home-grown one would
//     have to decide whether 127.0.0.1 and localhost (or 0.0.0.0 and "") are
//     the same bind, which is exactly the guesswork that rejects valid configs.
//     The residual collision spellings (:8080 vs 0.0.0.0:8080) fail at net.Listen
//     with a clear bind error, not silently. The one collision the kernel cannot
//     report is serve's own IPv6 twin (127.0.0.1:<port> also binds [::1]:<port>
//     for *.localhost, and that bind's failure is only logged), so an address
//     equal to the twin is refused here;
//   - max_file_bytes is positive and bounded: the read path buffers one file
//     per request, and the listener's pool is sized for the compiled ceiling —
//     a larger value would let a few requests pin unbounded memory;
//   - every index_files entry is a plain file name inside the target directory
//     (no separator, no "..", not empty).
//
// Every message names its key so the setup page marks the exact row.
func checkStaticSites(c *Config) []error {
	s := c.StaticSites
	if !s.Enabled {
		return nil
	}
	var errs []error
	if strings.TrimSpace(s.Host) == "" {
		errs = append(errs, fmt.Errorf("static_sites.host is required when static_sites.enabled = true (the only Host the static listener answers for, e.g. static.localhost)"))
	} else {
		if strings.TrimSpace(s.Host) != s.Host {
			// The handler compares the Host header exactly (case-insensitively,
			// port-insensitively): a stray space would make the listener answer
			// nobody.
			errs = append(errs, fmt.Errorf("static_sites.host %q has surrounding whitespace", s.Host))
		}
		if origin, key := appOriginHostname(c); origin != "" && hostnameOf(s.Host) == origin {
			errs = append(errs, fmt.Errorf("static_sites.host %q is %s's hostname %q: the static listener must not answer on the application origin, or a published site's JavaScript would run with the visitor's session (docs/features/19 §2)", s.Host, key, origin))
		}
	}
	if strings.TrimSpace(s.Address) == "" {
		errs = append(errs, fmt.Errorf("static_sites.address is required when static_sites.enabled = true (host:port, e.g. 127.0.0.1:8081)"))
	} else if _, _, err := net.SplitHostPort(s.Address); err != nil {
		errs = append(errs, fmt.Errorf("static_sites.address %q must be host:port (e.g. 127.0.0.1:8081)", s.Address))
	} else if s.Address == c.Server.Listen {
		errs = append(errs, fmt.Errorf("static_sites.address %q is server.listen: the static listener needs its own address, or two listeners would fight for one socket", s.Address))
	} else if twin, ok := loopbackTwinListen(c.Server.Listen); ok && s.Address == twin {
		errs = append(errs, fmt.Errorf("static_sites.address %q is the IPv6 twin of server.listen %q: serve binds both for *.localhost, so this one would silently fail to bind", s.Address, c.Server.Listen))
	}
	if s.MaxFileBytes <= 0 {
		errs = append(errs, fmt.Errorf("static_sites.max_file_bytes must be positive (default 8MiB)"))
	} else if s.MaxFileBytes > staticSitesMaxFileBytes {
		errs = append(errs, fmt.Errorf("static_sites.max_file_bytes is %s; the ceiling is 64MiB (each request buffers one file, bounded by the listener's pool)", s.MaxFileBytes))
	}
	for i, name := range s.IndexFiles {
		if err := validateStaticIndexFile(name); err != nil {
			errs = append(errs, fmt.Errorf("static_sites.index_files[%d]: %v", i, err))
		}
	}
	return errs
}

// staticSitesMaxFileBytes is the ceiling for static_sites.max_file_bytes
// (docs/features/19 §5). The read path buffers a whole file per in-flight
// request, so the cap is the per-request memory bound the listener's pool is
// sized for; the 8 MiB default stays unchanged.
const staticSitesMaxFileBytes = 64 << 20

// appOriginHostname reports the hostname the application answers on and the
// config key it came from: server.public_url's hostname when that parses to
// one, else server.listen's hostname when it names a real host (never the
// wildcard 0.0.0.0/::, which binds every address rather than one name).
// Lower-case and port-insensitive, like the static handler's Host comparison.
func appOriginHostname(c *Config) (host, key string) {
	if u, err := url.Parse(strings.TrimSpace(c.Server.PublicURL)); err == nil && u.Host != "" {
		if h := strings.ToLower(u.Hostname()); h != "" {
			return h, "server.public_url"
		}
	}
	h, _, err := net.SplitHostPort(strings.TrimSpace(c.Server.Listen))
	if err != nil {
		return "", ""
	}
	h = hostnameOf(h)
	switch h {
	case "", "0.0.0.0", "::":
		return "", ""
	}
	return h, "server.listen"
}

// hostnameOf normalises one configured host spelling for comparison: trimmed,
// lower-case, without a port and without IPv6 brackets.
func hostnameOf(h string) string {
	h = strings.TrimSpace(h)
	if hv, _, err := net.SplitHostPort(h); err == nil {
		h = hv
	}
	return strings.Trim(strings.ToLower(h), "[]")
}

// loopbackTwinListen mirrors cmd/spindle's loopbackTwin: the IPv6 twin the
// serve path binds when server.listen is a 127.0.0.1 bind. The two must agree —
// a collision the twin bind cannot report (it is only logged) is refused here.
func loopbackTwinListen(listen string) (string, bool) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(listen))
	if err != nil || host != "127.0.0.1" {
		return "", false
	}
	return net.JoinHostPort("::1", port), true
}

// validateStaticIndexFile checks one index_files entry: a single path segment
// naming a file in the probed directory. A separator or ".." would make the
// index probe escape its directory, which is the traversal the resolver refuses
// at request time — refusing it here means the operator hears about it at save.
func validateStaticIndexFile(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("must not be empty (give a file name such as index.html)")
	case strings.TrimSpace(name) != name:
		return fmt.Errorf("%q has surrounding whitespace", name)
	case strings.ContainsAny(name, "/\\"):
		return fmt.Errorf("%q must be a plain file name, not a path (it is probed inside the requested directory)", name)
	case strings.Contains(name, ".."):
		return fmt.Errorf("%q must not contain '..'", name)
	case name == ".":
		return fmt.Errorf("%q names the directory, not a file", name)
	}
	return nil
}

// AdvertisedSSHPort is the port advertised in ssh:// clone URLs (17_ssh.md
// §3): server.ssh.external_port when set, else the port parsed from
// server.ssh.listen. 0 means there is no port to advertise (SSH disabled and
// no external override, or an unparseable listen address) — callers omit the
// advertisement and fall back to the port-less derivation.
func (s ServerSSH) AdvertisedSSHPort() int {
	if s.ExternalPort < 0 || s.ExternalPort > 65535 {
		return 0
	}
	if s.ExternalPort != 0 {
		return s.ExternalPort
	}
	_, port, err := net.SplitHostPort(s.Listen)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return 0
	}
	return n
}

// Removed auth keys: a file (or env overlay) that still carries one is a
// named error rather than a generic "unknown key" — the operator needs to know
// what replaced it (PLAN-OIDC-ONLY §4.1).
const (
	removedAuthModeHint     = "server.auth.mode was removed — OIDC (issuer + client + session secret) is the only authentication; delete the key"
	removedAdminEmailsHint  = "server.auth.admin_emails was removed — administration is the OIDC admin group (server.auth.admin_groups)"
	removedAdminDomainsHint = "server.auth.admin_domains was removed — administration is the OIDC admin group (server.auth.admin_groups)"
	removedTokenAdminHint   = "server.auth.tokens[].admin was removed — static tokens are machine credentials and can never administer; administration is granted only by the OIDC admin group (server.auth.admin_groups)"
)

// removedAuthKeyHint maps a residual auth key onto its removal message.
func removedAuthKeyHint(key string) (string, bool) {
	switch key {
	case "server.auth.mode":
		return removedAuthModeHint, true
	case "server.auth.admin_emails":
		return removedAdminEmailsHint, true
	case "server.auth.admin_domains":
		return removedAdminDomainsHint, true
	}
	if strings.HasPrefix(key, "server.auth.tokens") && strings.HasSuffix(key, ".admin") {
		return removedTokenAdminHint, true
	}
	return "", false
}

// checkAuthConfigured is the always-on authentication rule (PLAN-OIDC-ONLY
// §4.2). Auth configured ⇔ issuer ∧ client id/secret ∧ session secret (≥32 B)
// ∧ an admission allowlist ∧ groups_claim ∧ admin_groups. Every finding names
// its key, so `spindle config check` and the restricted /admin page can point
// at the missing field.
func checkAuthConfigured(c *Config) []error {
	a := c.Server.Auth
	var errs []error
	issuer := strings.TrimSpace(a.Issuer)
	switch {
	case issuer == "":
		errs = append(errs, fmt.Errorf("server.auth.issuer is required — OIDC discovery needs it (e.g. https://id.example.com)"))
	default:
		if u, err := url.Parse(issuer); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, fmt.Errorf("server.auth.issuer %q must be an absolute http(s) URL (e.g. https://id.example.com)", a.Issuer))
		}
	}
	if len(a.AllowedDomains) == 0 && len(a.AllowedEmails) == 0 {
		errs = append(errs, fmt.Errorf("server.auth.allowed_domains or server.auth.allowed_emails is required — without an admission rule nobody may sign in"))
	}
	if strings.TrimSpace(a.GroupsClaim) == "" {
		errs = append(errs, fmt.Errorf("server.auth.groups_claim is required (default \"groups\") — it names the ID-token claim carrying group membership"))
	}
	if len(a.AdminGroups) == 0 {
		errs = append(errs, fmt.Errorf("server.auth.admin_groups must not be empty — administration is granted only by an OIDC group (default [\"spindle-admin\"]); an empty list is a guaranteed lockout"))
	} else {
		nonEmpty := false
		for _, g := range a.AdminGroups {
			if strings.TrimSpace(g) != "" {
				nonEmpty = true
			}
		}
		if !nonEmpty {
			errs = append(errs, fmt.Errorf("server.auth.admin_groups must name at least one non-empty group (default [\"spindle-admin\"])"))
		}
	}
	var missing []string
	if a.SessionSecret == "" {
		missing = append(missing, "server.auth.session_secret")
	}
	if a.OAuthClientID == "" {
		missing = append(missing, "server.auth.oauth_client_id")
	}
	if a.OAuthClientSecret == "" {
		missing = append(missing, "server.auth.oauth_client_secret")
	}
	if len(missing) > 0 {
		errs = append(errs, fmt.Errorf("OIDC requires %s — sign-in is impossible without all three (set them in the config file or with SPINDLE__SERVER__AUTH__* env vars)", strings.Join(missing, ", ")))
	}
	if a.SessionSecret != "" && len(a.SessionSecret) < 32 {
		errs = append(errs, fmt.Errorf("server.auth.session_secret must be at least 32 bytes (got %d)", len(a.SessionSecret)))
	}
	return errs
}

// checkStaticTokens validates the machine-credential rows (06 §8.2.1). An
// `expires_at` must be RFC 3339; a past instant is deliberately accepted (the
// credential is simply already expired) so a config saved before the deadline
// keeps validating after it. Names are the rotation/usage key, so a duplicate
// would make the Access table and the last-used record ambiguous.
func checkStaticTokens(c *Config) []error {
	var errs []error
	seen := map[string]int{}
	for i, t := range c.Server.Auth.Tokens {
		at := fmt.Sprintf("server.auth.tokens[%d]", i)
		name := strings.TrimSpace(t.Name)
		if name != "" {
			if j, dup := seen[name]; dup {
				errs = append(errs, fmt.Errorf("%s.name %q is used twice (also server.auth.tokens[%d]) — a name identifies one credential for rotation and last-used tracking", at, name, j))
			} else {
				seen[name] = i
			}
		}
		if exp := strings.TrimSpace(t.ExpiresAt); exp != "" {
			if _, err := time.Parse(time.RFC3339, exp); err != nil {
				errs = append(errs, fmt.Errorf("%s.expires_at must be an RFC 3339 timestamp (e.g. 2026-12-31T23:59:59Z): %v", at, err))
			}
		}
	}
	return errs
}

// 3. bundle strategy validation.
func checkBundleStrategies(c *Config) []error {
	var errs []error
	for i, s := range c.Bundles.Strategy {
		at := fmt.Sprintf("bundles.strategy[%d]", i)
		if s.Name == "" {
			errs = append(errs, fmt.Errorf("%s.name is required", at))
		} else {
			for j := range i {
				if c.Bundles.Strategy[j].Name == s.Name {
					errs = append(errs, fmt.Errorf("%s.name %q duplicates bundles.strategy[%d]", at, s.Name, j))
				}
			}
		}
		switch s.Kind {
		case "full":
		case "incremental":
			if s.Base == "" {
				errs = append(errs, fmt.Errorf("%s: incremental strategy requires base", at))
			} else {
				found := false
				for j := range i {
					if c.Bundles.Strategy[j].Name == s.Base {
						found = true
						break
					}
				}
				if !found {
					errs = append(errs, fmt.Errorf("%s: base %q does not name an earlier-declared strategy", at, s.Base))
				}
			}
			if s.Keep != 0 {
				errs = append(errs, fmt.Errorf("%s: keep is a full-strategy concept and fails on an incremental strategy", at))
			}
		default:
			errs = append(errs, fmt.Errorf("%s.kind must be \"full\" or \"incremental\" (got %q)", at, s.Kind))
		}
		if !validCron(s.Schedule) {
			errs = append(errs, fmt.Errorf("%s.schedule %q is not a valid 6-field cron expression or @macro", at, s.Schedule))
		}
		if s.Filter != "" && s.Filter != "blob:none" {
			errs = append(errs, fmt.Errorf("%s.filter must be \"blob:none\" (got %q)", at, s.Filter))
		}
		if s.BackfillMax < 0 {
			errs = append(errs, fmt.Errorf("%s.backfill_max must be >= 0", at))
		}
		if s.MinCommits < 0 {
			errs = append(errs, fmt.Errorf("%s.min_commits must be >= 0", at))
		}
		for _, ref := range s.Refs {
			if !validRefGlob(ref) {
				errs = append(errs, fmt.Errorf("%s.refs glob %q does not compile", at, ref))
			}
		}
	}
	return errs
}

// 4. chain-shared filter: a strategy and its transitive base chain MUST
// declare identical filter values (absent = none); mixing is rejected by name.
func checkBundleChainFilters(c *Config) []error {
	var errs []error
	strategies := c.Bundles.Strategy
	for i, s := range strategies {
		filters := map[string]bool{s.Filter: true}
		name := s.Base
		for range len(strategies) {
			if name == "" {
				break
			}
			var base *BundleStrategy
			for j := range i {
				if strategies[j].Name == name {
					base = &strategies[j]
					break
				}
			}
			if base == nil {
				break
			}
			filters[base.Filter] = true
			name = base.Base
		}
		if len(filters) > 1 {
			errs = append(errs, fmt.Errorf("bundles.strategy[%d] (%s) mixes filter values in its base chain: a filtered chain is all-or-nothing", i, s.Name))
		}
	}
	return errs
}

// 5. store.
func checkStore(c *Config) []error {
	var errs []error
	switch c.Store.Backend {
	case "s3", "memory", "filesystem":
	default:
		errs = append(errs, fmt.Errorf("store.backend must be one of s3|memory|filesystem (got %q)", c.Store.Backend))
	}
	// Selecting the bucket backend is explicit, and an incomplete selection
	// fails here rather than half-working against an unintended endpoint
	// (OBS-1: the compiled default is no longer an implicit AWS endpoint).
	if c.Store.Backend == "s3" {
		if strings.TrimSpace(c.Store.S3.Endpoint) == "" {
			errs = append(errs, fmt.Errorf("store.s3.endpoint is required when store.backend = \"s3\" (set the S3-compatible endpoint URL, or leave store.backend = \"filesystem\")"))
		}
		if c.Store.Bucket == "" {
			errs = append(errs, fmt.Errorf("store.bucket is required when store.backend = \"s3\""))
		}
		if c.Store.S3.AccessKeyEnv == "" {
			errs = append(errs, fmt.Errorf("store.s3.access_key_env is required when store.backend = \"s3\" (the name of the env var holding the access key)"))
		}
		if c.Store.S3.SecretKeyEnv == "" {
			errs = append(errs, fmt.Errorf("store.s3.secret_key_env is required when store.backend = \"s3\" (the name of the env var holding the secret key)"))
		}
	}
	if c.Store.MultipartPartSize > 0 && c.Store.MultipartThreshold > 0 &&
		c.Store.MultipartPartSize > c.Store.MultipartThreshold {
		errs = append(errs, fmt.Errorf("store.multipart_part_size (%s) must be <= store.multipart_threshold (%s)",
			c.Store.MultipartPartSize, c.Store.MultipartThreshold))
	}
	if c.Store.Backend == "filesystem" && c.Store.Root != "" && !strings.HasPrefix(c.Store.Root, "/") {
		errs = append(errs, fmt.Errorf("store.root must be an absolute path or empty (got %q)", c.Store.Root))
	}
	return errs
}

// 6. sizes/timeouts: negative values nowhere (durations/sizes already parsed
// by the typed decoders), watermark in (0,1) or 0.
func checkSizes(c *Config) []error {
	var errs []error
	var walk func(rv reflect.Value, path string)
	walk = func(rv reflect.Value, path string) {
		rt := rv.Type()
		for i := range rt.NumField() {
			f := rt.Field(i)
			tag, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
			if tag == "-" {
				continue
			}
			at := tag
			if path != "" {
				at = path + "." + tag
			}
			fv := rv.Field(i)
			switch fv.Kind() {
			case reflect.Struct:
				walk(fv, at)
			case reflect.Int64:
				switch fv.Type() {
				case reflect.TypeOf(Duration(0)):
					if Duration(fv.Int()) < 0 {
						errs = append(errs, fmt.Errorf("negative value for %s", at))
					}
				case reflect.TypeOf(ByteSize(0)):
					if ByteSize(fv.Int()) < 0 {
						errs = append(errs, fmt.Errorf("negative value for %s", at))
					}
				}
			}
		}
	}
	walk(reflect.ValueOf(c).Elem(), "")
	if w := c.Cache.DiskHighWatermark; w != 0 && (w <= 0 || w >= 1) {
		errs = append(errs, fmt.Errorf("cache.disk_high_watermark must be in (0,1) or 0 (got %v)", w))
	}
	return errs
}

// 7. placement globs: `*`, `owner/*`, or `owner/name` (one / at most).
func checkPlacementGlobs(c *Config) []error {
	var errs []error
	lists := [...]struct {
		key  string
		vals []string
	}{
		{"placement.serve", c.Placement.Serve},
		{"placement.serve_exclude", c.Placement.ServeExclude},
		{"placement.maintain", c.Placement.Maintain},
		{"placement.maintain_exclude", c.Placement.MaintainExclude},
	}
	for _, l := range lists {
		for _, g := range l.vals {
			if !validPlacementGlob(g) {
				errs = append(errs, fmt.Errorf("%s glob %q must be *, owner/*, or owner/name", l.key, g))
			}
		}
	}
	return errs
}

func validPlacementGlob(g string) bool {
	if g == "" || strings.ContainsAny(g, "?[]\\") {
		return false
	}
	owner, pattern, hasSlash := strings.Cut(g, "/")
	if hasSlash {
		if strings.Contains(pattern, "/") {
			return false // one / at most
		}
		if owner == "" {
			return false
		}
		return pattern == "*" || !strings.ContainsAny(pattern, "*")
	}
	return g == "*"
}

// 8. roles.
func checkRoles(c *Config) []error {
	var errs []error
	for _, r := range c.Server.Roles {
		switch r {
		case "serve", "maintain", "events":
		default:
			errs = append(errs, fmt.Errorf("server.roles entry %q must be one of serve|maintain|events", r))
		}
	}
	return errs
}

// 9. paths: cache.dir absolute unless the store backend is memory.
func checkPaths(c *Config) []error {
	if c.Store.Backend == "memory" {
		return nil
	}
	if !strings.HasPrefix(c.Cache.Dir, "/") {
		return []error{fmt.Errorf("cache.dir must be an absolute path (got %q)", c.Cache.Dir)}
	}
	return nil
}

// --- schedule / glob syntax (parse-only; the real cron parser lives in
// 08_bundles.md / internal/bundle) ---

var cronFieldRe = regexp.MustCompile(`^\*(/[0-9]+)?$|^([0-9]+|[A-Za-z]{3})(-([0-9]+|[A-Za-z]{3}))?(/[0-9]+)?$`)

// validCron reports whether schedule parses as a 6-field UTC cron expression
// or one of the @macros.
func validCron(s string) bool {
	switch s {
	case "@hourly", "@daily", "@weekly", "@monthly", "@yearly":
		return true
	}
	fields := strings.Fields(s)
	if len(fields) != 6 {
		return false
	}
	for _, f := range fields {
		for _, alt := range strings.Split(f, ",") {
			if !cronFieldRe.MatchString(alt) {
				return false
			}
		}
	}
	return true
}

// validRefGlob checks a git ref glob (owner-free) compiles: non-empty,
// balanced brackets, no stray escapes.
func validRefGlob(g string) bool {
	if g == "" {
		return false
	}
	depth := 0
	for _, r := range g {
		switch r {
		case '[':
			depth++
		case ']':
			depth--
			if depth < 0 {
				return false
			}
		case '\\':
			return false
		}
	}
	return depth == 0
}

// checkImport validates the additive [import] section (docs/features/10
// §6): positive bounds, and allowlist entries that are plain hosts
// (no scheme, no path, no port — matched against the source URL host).
func checkImport(c *Config) []error {
	var errs []error
	im := c.Import
	if im.MaxRefs < 1 {
		errs = append(errs, fmt.Errorf("import.max_refs must be >= 1 (got %d)", im.MaxRefs))
	}
	if im.MaxConcurrent < 1 {
		errs = append(errs, fmt.Errorf("import.max_concurrent must be >= 1 (got %d)", im.MaxConcurrent))
	}
	for _, h := range im.URLAllowlist {
		if h == "" || strings.ContainsAny(h, "/:@?#") || strings.Contains(h, " ") {
			errs = append(errs, fmt.Errorf("import.url_allowlist entry %q must be a plain host", h))
		}
	}
	return errs
}

// checkTreeLog validates the additive server.max_tree_log bound (issue
// #301): the per-entry tree-date walk cap, mirroring the [import] bounds
// pattern (positive bound, fail closed on garbage).
func checkTreeLog(c *Config) []error {
	if c.Server.MaxTreeLog < 1 {
		return []error{fmt.Errorf("server.max_tree_log must be >= 1 (got %d)", c.Server.MaxTreeLog)}
	}
	return nil
}
