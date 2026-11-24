package config

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

// MaxRepoSettingsBytes is the serialized-settings payload budget (§4.2):
// larger payloads are rejected (400 at publish).
const MaxRepoSettingsBytes = 16 << 10

// MaxRepoDescriptionRunes bounds the per-repo short description (issue #235):
// a header-line display string, not a document. Single-line (no CR/LF);
// longer or multi-line payloads are rejected (400 at publish) like any other
// invalid settings key.
const MaxRepoDescriptionRunes = 512

// RepoSettings are the per-repo settings published into the WAL (D24): the
// allowed sections merged over the host config by Merge ("with_settings").
// Zero pointers mean "not set, inherit host config". Description is display
// metadata (issue #235): it rides the same TOML document — persistence,
// revisioning, authorship, and admin-only writes free — but it never merges
// into the host config (Merge ignores it). Features (Forgejo #522) is the
// same kind of passenger: six per-repo feature flags validated, persisted,
// and revisioned with the doc, but never merged (there is no host-config
// counterpart — an absent section means every feature enabled). Review
// (Forgejo #586) is the same kind of passenger again: the [review] section
// carries the allow_self_approval knob (absent key means allowed — fresh
// repos behave like GitHub), never merged (no host-config counterpart).
// Packages is the fourth: [packages] carries the registry quota override and
// retention rules, never merged — the host supplies only the default quota
// (PackagesPolicy resolves the pair; docs/features/14 §5).
// StaticSite is the fifth passenger: [static_site] says whether the repository
// is published on the static host and from which ref/subdirectory
// (docs/features/19 §5), never merged — per-repo publication is not a
// host-config default, and the host section [static_sites] configures the
// listener instead.
type RepoSettings struct {
	Description  string                    `toml:"description"`
	Features     *RepoFeatures             `toml:"features"`
	Review       *RepoReview               `toml:"review"`
	Packages     *RepoPackages             `toml:"packages"`
	StaticSite   *RepoStaticSite           `toml:"static_site"`
	Bundles      *Bundles                  `toml:"bundles"`
	Maintenance  *Maintenance              `toml:"maintenance"`
	Compaction   *Compaction               `toml:"compaction"`
	Upstream     *Upstream                 `toml:"upstream"`
	Integrations map[string]toml.Primitive `toml:"integrations"` // accepted, forward-compat, never interpreted
}

// RepoFeatures are the six per-repo feature flags (Forgejo #522), carried
// as the [features] TOML section. Every key is an *enabled* flag: nil (key
// absent) means enabled, so an absent section — every existing repo —
// resolves to all-on with zero migration. Pointers (not plain bools) are
// what keep "unset" distinct from "explicitly disabled": a plain bool
// would read an absent key as false and strand every pre-#522 repo with
// all features off.
type RepoFeatures struct {
	Issues   *bool `toml:"issues"`
	Pulls    *bool `toml:"pulls"`
	Releases *bool `toml:"releases"`
	Forks    *bool `toml:"forks"`
	Watch    *bool `toml:"watch"`
	Star     *bool `toml:"star"`
	// Packages is the OCI registry toggle (docs/features/14 §7): off ⇒ the
	// tab disappears and every /v2/<o>/<r>/* route answers 404 (fail closed,
	// like the other flags). Default on, like its siblings.
	Packages *bool `toml:"packages"`
}

// ResolvedFeatures are the effective flags: plain bools, always fully
// populated (absent keys resolved to enabled). This is what the summary
// projects and what the write guards consult — never a nil-meaning
// question at a read or write boundary.
type ResolvedFeatures struct {
	Issues   bool `json:"issues"`
	Pulls    bool `json:"pulls"`
	Releases bool `json:"releases"`
	Forks    bool `json:"forks"`
	Watch    bool `json:"watch"`
	Star     bool `json:"star"`
	Packages bool `json:"packages"`
}

// AllFeatures is the zero-migration default: every feature enabled.
func AllFeatures() ResolvedFeatures {
	return ResolvedFeatures{Issues: true, Pulls: true, Releases: true, Forks: true, Watch: true, Star: true, Packages: true}
}

// Resolve maps the [features] section onto effective flags: nil section
// or nil key means enabled; an explicit false disables.
func (f *RepoFeatures) Resolve() ResolvedFeatures {
	out := AllFeatures()
	if f == nil {
		return out
	}
	if f.Issues != nil {
		out.Issues = *f.Issues
	}
	if f.Pulls != nil {
		out.Pulls = *f.Pulls
	}
	if f.Releases != nil {
		out.Releases = *f.Releases
	}
	if f.Forks != nil {
		out.Forks = *f.Forks
	}
	if f.Watch != nil {
		out.Watch = *f.Watch
	}
	if f.Star != nil {
		out.Star = *f.Star
	}
	if f.Packages != nil {
		out.Packages = *f.Packages
	}
	return out
}

// ETagBits renders the resolved flags as seven fixed-order 0/1 chars
// (issues, pulls, releases, forks, watch, star, packages): the short
// summary-ETag suffix covering a settings-only flip with no ref move (the
// #235/#505 suffix discipline). Fixed order and fixed length — always seven
// chars, so the suffix is stable and greppable. Packages joined the set with
// docs/features/14 (the 2026-11-20 addition): a registry toggle must bust the
// cache or the tab stays hidden behind the SWR window.
func (f ResolvedFeatures) ETagBits() string {
	bits := []byte{'1', '1', '1', '1', '1', '1', '1'}
	if !f.Issues {
		bits[0] = '0'
	}
	if !f.Pulls {
		bits[1] = '0'
	}
	if !f.Releases {
		bits[2] = '0'
	}
	if !f.Forks {
		bits[3] = '0'
	}
	if !f.Watch {
		bits[4] = '0'
	}
	if !f.Star {
		bits[5] = '0'
	}
	if !f.Packages {
		bits[6] = '0'
	}
	return string(bits)
}

// FeaturesOf extracts the resolved feature flags from a stored settings
// TOML body, returning all-enabled for empty bodies and for bodies that
// no longer parse (display metadata must never fail a read path — the
// summary renders all-on then, exactly like DescriptionOf renders "").
func FeaturesOf(body []byte) ResolvedFeatures {
	if len(bytes.TrimSpace(body)) == 0 {
		return AllFeatures()
	}
	rs, err := ParseRepoSettings(body)
	if err != nil {
		return AllFeatures()
	}
	return rs.Features.Resolve()
}

// RepoReview is the [review] settings section (Forgejo #586): the
// per-repo code-review policy knobs. Every key is a pointer so "unset"
// stays distinct from "explicitly false": a plain bool would read an
// absent key as false and strand every pre-#586 repo with self-approval
// off (the #522 pointer discipline — an absent section means the
// GitHub-like default, allowed).
type RepoReview struct {
	AllowSelfApproval *bool `toml:"allow_self_approval"`
}

// Allowed resolves the effective flag: nil section or nil key
// means allowed (fresh repos behave like GitHub — the author may
// APPROVE/CHANGES_REQUESTED their own pull request; the required-reviews
// merge gate still never counts a self-approval toward min_approvals).
func (r *RepoReview) Allowed() bool {
	if r == nil || r.AllowSelfApproval == nil {
		return true
	}
	return *r.AllowSelfApproval
}

// AllowSelfApprovalOf extracts the effective self-approval flag from a
// stored settings TOML body, returning allowed (true) for empty bodies
// and for bodies that no longer parse (the FeaturesOf fail-open
// precedent: an unparseable body can never be published — publish
// validates — so a read path must never fail on it; it renders the
// default, exactly like DescriptionOf renders "").
func AllowSelfApprovalOf(body []byte) bool {
	if len(bytes.TrimSpace(body)) == 0 {
		return true
	}
	rs, err := ParseRepoSettings(body)
	if err != nil {
		return true
	}
	return rs.Review.Allowed()
}

// RepoPackages is the per-repository [packages] policy (docs/features/14 §5):
// the OCI registry quota and the retention rules the maintainer's
// `packages-gc` sweep applies. It is a behavior passenger like [features] —
// carried, validated and revisioned with the settings document, never merged
// into the host config (the host only supplies the instance quota default,
// `packages.quota_bytes`, which PackagesPolicy resolves against).
type RepoPackages struct {
	// QuotaBytes overrides the instance default quota. nil = inherit
	// (packages.quota_bytes); an explicit 0 = unlimited for this
	// repository, which is why it is a pointer and not a ByteSize (the
	// #522 "unset ≠ explicit zero" discipline).
	QuotaBytes *ByteSize `toml:"quota_bytes"`
	// Retention is the tag retention policy: one rule per tag pattern,
	// applied by the maintainer pass. Empty = keep every tag (the
	// compatible default).
	Retention []PackageRetention `toml:"retention"`
}

// PackageRetention is one retention rule. Pattern is a glob over the tag name
// (path.Match syntax; "" or "*" matches every tag). KeepLast keeps the newest
// N matching tags; MaxAge keeps tags whose last update is younger than it.
// At least one of the two must be set; the first matching rule wins.
type PackageRetention struct {
	Pattern  string   `toml:"pattern"`
	KeepLast int      `toml:"keep_last"`
	MaxAge   Duration `toml:"max_age"`
}

// PackagesPolicy is the resolved per-repository registry policy: the numbers
// the packages service enforces, never a nil-meaning question at the commit
// point.
type PackagesPolicy struct {
	QuotaBytes int64
	Retention  []PackageRetention
}

// MaxPackageRetentionRules bounds the rule list: every rule is a glob tested
// per tag on every maintainer pass, and a document with thousands of rules is
// a typo, not a policy.
const MaxPackageRetentionRules = 16

// ValidateRepoPackages checks the [packages] policy at publish time (fail
// closed: a negative quota or a rule that can never keep anything is
// rejected, never silently ignored).
func ValidateRepoPackages(p *RepoPackages) error {
	if p == nil {
		return nil
	}
	if p.QuotaBytes != nil && *p.QuotaBytes < 0 {
		return errors.New("repo settings: packages.quota_bytes must be zero (unlimited) or positive")
	}
	if len(p.Retention) > MaxPackageRetentionRules {
		return fmt.Errorf("repo settings: packages.retention has %d rules; limit is %d",
			len(p.Retention), MaxPackageRetentionRules)
	}
	for i, r := range p.Retention {
		if _, err := path.Match(matchAllIfEmpty(r.Pattern), "x"); err != nil {
			return fmt.Errorf("repo settings: packages.retention[%d].pattern %q is not a glob", i, r.Pattern)
		}
		if r.KeepLast < 0 {
			return fmt.Errorf("repo settings: packages.retention[%d].keep_last must not be negative", i)
		}
		if r.MaxAge < 0 {
			return fmt.Errorf("repo settings: packages.retention[%d].max_age must not be negative", i)
		}
		if r.KeepLast == 0 && r.MaxAge == 0 {
			return fmt.Errorf("repo settings: packages.retention[%d] sets neither keep_last nor max_age", i)
		}
	}
	return nil
}

// matchAllIfEmpty maps the "no pattern" spellings onto the glob that matches
// everything, so "" and "*" mean the same rule.
func matchAllIfEmpty(pattern string) string {
	if pattern == "" {
		return "*"
	}
	return pattern
}

// Pattern for a rule as the matcher sees it ("" and "*" both match all).
func (r PackageRetention) Match(tag string) bool {
	ok, err := path.Match(matchAllIfEmpty(r.Pattern), tag)
	return err == nil && ok
}

// PackagesPolicy resolves the policy: the repo settings override the instance
// default quota; retention is per-repository only (the host has no retention
// default — an instance-wide delete policy is an operator surprise nobody
// asked for). An unparseable or absent section resolves to the default
// (fail-open display rule, like FeaturesOf: an unreadable settings doc must
// never make the registry refuse pushes on a quota nobody set).
func (rs *RepoSettings) PackagesPolicy(defaults Packages) PackagesPolicy {
	out := PackagesPolicy{QuotaBytes: int64(defaults.QuotaBytes)}
	if rs == nil || rs.Packages == nil {
		return out
	}
	if q := rs.Packages.QuotaBytes; q != nil {
		out.QuotaBytes = int64(*q)
	}
	out.Retention = append(out.Retention, rs.Packages.Retention...)
	return out
}

// PackagesPolicyOf extracts the resolved policy from a stored settings TOML
// body (the one call the composition projection makes).
func PackagesPolicyOf(body []byte, defaults Packages) PackagesPolicy {
	if len(bytes.TrimSpace(body)) == 0 {
		return PackagesPolicy{QuotaBytes: int64(defaults.QuotaBytes)}
	}
	rs, err := ParseRepoSettings(body)
	if err != nil {
		return PackagesPolicy{QuotaBytes: int64(defaults.QuotaBytes)}
	}
	return rs.PackagesPolicy(defaults)
}

// RepoStaticSite is the [static_site] publication section (docs/features/19
// §5): whether this repository is served by the dedicated static host, and
// where its site root sits. It is a behavior passenger like [features] —
// carried, validated and revisioned with the settings document, never merged
// into the host config (the listener itself is the host section
// `[static_sites]`; there is no per-repo default to inherit).
//
// Enabled is a pointer for the #522 reason, with a different resolution: an
// absent key and an explicit false both mean "not published" (publication is
// opt-in), but they stay distinguishable so the settings summary and the diff
// can say "unset" instead of reporting a value nobody wrote.
type RepoStaticSite struct {
	Enabled *bool  `toml:"enabled"`
	Ref     string `toml:"ref"`  // "" = the repository's default branch tip
	Root    string `toml:"root"` // "" = the repository root
}

// ResolvedStaticSite is the effective publication state: plain values, never a
// nil-meaning question at the handler. Enabled false means the repository is
// not published at all (its ref/root are irrelevant then).
type ResolvedStaticSite struct {
	Enabled bool
	Ref     string
	Root    string
}

// Resolve maps [static_site] onto the effective state: nil section, nil key or
// explicit false all mean not published.
func (s *RepoStaticSite) Resolve() ResolvedStaticSite {
	if s == nil || s.Enabled == nil || !*s.Enabled {
		return ResolvedStaticSite{}
	}
	return ResolvedStaticSite{Enabled: true, Ref: s.Ref, Root: s.Root}
}

// StaticSiteOf extracts the effective publication state from a stored settings
// TOML body. Empty, unparseable and invalid bodies all resolve to "not
// published": unlike the display helpers (FeaturesOf/DescriptionOf fail open to
// the friendlier default), the safe direction here is closed — an unreadable
// document must never expose a repository's bytes on another origin.
func StaticSiteOf(body []byte) ResolvedStaticSite {
	if len(bytes.TrimSpace(body)) == 0 {
		return ResolvedStaticSite{}
	}
	rs, err := ParseRepoSettings(body)
	if err != nil {
		return ResolvedStaticSite{}
	}
	return rs.StaticSite.Resolve()
}

// ValidateRepoStaticSite checks the [static_site] section at publish time (fail
// closed: a ref or root that can never resolve is rejected, never silently
// served as the wrong tree).
//
// The root rule is the handler's cleanRoot (internal/staticpages/path.go) in
// this package's words: the two are one contract. A root the handler refuses
// would publish a site that is a permanent 404, and the save would have looked
// successful — so the refusal belongs here, at the moment the operator can
// still correct it, naming the key.
func ValidateRepoStaticSite(s *RepoStaticSite) error {
	if s == nil {
		return nil
	}
	if ref := s.Ref; ref != "" && !validGitRefName(ref) {
		return fmt.Errorf("repo settings: static_site.ref %q is not a valid git ref name", ref)
	}
	if root := s.Root; root != "" {
		switch {
		case len(root) > 512:
			return fmt.Errorf("repo settings: static_site.root is %d bytes; the limit is 512", len(root))
		case strings.HasPrefix(root, "/"):
			return fmt.Errorf("repo settings: static_site.root %q must be relative to the repository root", root)
		case strings.HasSuffix(root, "/"):
			return fmt.Errorf("repo settings: static_site.root %q must not end with '/'", root)
		case strings.Contains(root, `\`):
			return fmt.Errorf("repo settings: static_site.root %q must not contain a backslash", root)
		}
		for i := 0; i < len(root); i++ {
			if c := root[i]; c < 0x20 || c == 0x7f {
				return fmt.Errorf("repo settings: static_site.root %q must not contain control bytes", root)
			}
		}
		for _, part := range strings.Split(root, "/") {
			switch {
			case part == "":
				return fmt.Errorf("repo settings: static_site.root %q has an empty path segment", root)
			case part == ".":
				return fmt.Errorf("repo settings: static_site.root %q: '.' names the directory itself (use \"\" for the repository root)", root)
			case part == "..":
				return fmt.Errorf("repo settings: static_site.root %q must not contain '..'", root)
			case part[0] == '.':
				return fmt.Errorf("repo settings: static_site.root %q has a dot-directory %q (dotfiles never serve)", root, part)
			}
		}
	}
	return nil
}

// validGitRefName reports whether name is a git-check-ref-format-compatible ref
// name: no control bytes, space, ~ ^ : ? * [ \ ; no "..", no "@{", no "//"; no
// component starting with "." or ending in ".lock"; no leading "-" (option
// confusion) or "/", no trailing "/" or ".". A short branch name ("main") and a
// full ref ("refs/heads/gh-pages") are both accepted — the resolver decides
// which namespace it means.
func validGitRefName(name string) bool {
	if name == "" || name == "@" || strings.HasPrefix(name, "-") {
		return false
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") {
		return false
	}
	if strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.Contains(name, "//") {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 0x20 || c == 0x7f {
			return false
		}
		switch c {
		case ' ', '~', '^', ':', '?', '*', '[', '\\':
			return false
		}
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

// hostOnlyRepoSettings sections produce a clearer error than "unknown".
var hostOnlyRepoSettings = map[string]bool{
	"server": true,
	"store":  true,
	"wal":    true,
	"cache":  true,
	"auth":   true,
}

// ParseRepoSettings validates and decodes a settings payload (§4.2): size
// budget, allowed sections only ([features] [review] [packages] [static_site]
// [bundles] [maintenance] [compaction] [upstream], plus the top-level
// description key and [integrations] stored verbatim), and host-only keys
func ParseRepoSettings(payload []byte) (*RepoSettings, error) {
	if len(payload) > MaxRepoSettingsBytes {
		return nil, fmt.Errorf("repo settings payload is %d bytes; limit is %d", len(payload), MaxRepoSettingsBytes)
	}
	var rs RepoSettings
	md, err := toml.NewDecoder(bytes.NewReader(payload)).Decode(&rs)
	if err != nil {
		return nil, fmt.Errorf("repo settings: %v", err)
	}
	if rs.Upstream != nil && rs.Upstream.TokenEnv != "" {
		return nil, errors.New("repo settings: upstream.token_env is host-only and not settable via settings")
	}
	if err := ValidateRepoDescription(rs.Description); err != nil {
		return nil, err
	}
	if err := ValidateRepoPackages(rs.Packages); err != nil {
		return nil, err
	}
	if err := ValidateRepoStaticSite(rs.StaticSite); err != nil {
		return nil, err
	}
	for _, key := range md.Undecoded() {
		head := key[0]
		if head == "integrations" {
			continue // stored verbatim, forward-compat, never interpreted
		}
		if hostOnlyRepoSettings[head] {
			return nil, fmt.Errorf("repo settings: %q is not settable via settings", head)
		}
		return nil, fmt.Errorf("repo settings: unknown key %q", key.String())
	}
	return &rs, nil
}

// ValidateRepoDescription enforces the issue-#235 description shape: a
// single-line display string of at most MaxRepoDescriptionRunes runes. The
// empty string means "unset" and is always valid.
func ValidateRepoDescription(d string) error {
	if strings.ContainsAny(d, "\r\n\x00") {
		return errors.New("repo settings: description must be a single line")
	}
	if n := utf8.RuneCountInString(d); n > MaxRepoDescriptionRunes {
		return fmt.Errorf("repo settings: description is %d characters; limit is %d", n, MaxRepoDescriptionRunes)
	}
	return nil
}

// DescriptionOf extracts the description from a stored settings TOML body,
// returning "" for empty bodies and for bodies that no longer parse (display
// metadata must never fail a read path — the summary renders "" then).
func DescriptionOf(body []byte) string {
	if len(bytes.TrimSpace(body)) == 0 {
		return ""
	}
	rs, err := ParseRepoSettings(body)
	if err != nil {
		return ""
	}
	return rs.Description
}

// Merge ("with_settings"): pointer-set fields override base; unset sections
// inherit. upstream.token_env never comes from settings — the base value is
// preserved. Description, Features, Review, Packages and StaticSite never
// merge: they are display/behavior metadata, not config (the host only
// defaults the packages quota, resolved separately by PackagesPolicy; the
// static listener is the host section [static_sites], while [static_site]
// only says what this repository publishes).
func (r *RepoSettings) Merge(base *Config) (*Config, error) {
	c := *base
	if r.Bundles != nil {
		c.Bundles = *r.Bundles
	}
	if r.Maintenance != nil {
		c.Maintenance = *r.Maintenance
	}
	if r.Compaction != nil {
		c.Compaction = *r.Compaction
	}
	if r.Upstream != nil {
		u := base.Upstream
		if r.Upstream.Git != "" {
			u.Git = r.Upstream.Git
		}
		if r.Upstream.Lfs != "" {
			u.Lfs = r.Upstream.Lfs
		}
		if len(r.Upstream.Follow) != 0 {
			u.Follow = r.Upstream.Follow
		}
		c.Upstream = u
	}
	// Integrations are stored verbatim and never interpreted.
	// StaticSite, like Description/Features/Review/Packages, rides the document
	// but contributes nothing to the host config: the listener is host-wide
	// ([static_sites]) and publication is resolved per repository.
	return &c, nil
}

// ValidateAgainst runs the host build's with_settings merge + full §5
// validation against the would-be effective config (§4.2): a settings payload
// must pass the same fail-closed validation as a host config, restricted to
// the merged sections.
func (r *RepoSettings) ValidateAgainst(base *Config) error {
	merged, err := r.Merge(base)
	if err != nil {
		return err
	}
	_, errs := Validate(merged)
	if len(errs) == 0 {
		return nil
	}
	msgs := make([]string, len(errs))
	for i, e := range errs {
		msgs[i] = e.Error()
	}
	return errors.New(strings.Join(msgs, "; "))
}
