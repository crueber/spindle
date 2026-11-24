// Package doctor implements `spindle doctor`: the read-only, bounded readiness
// check an operator runs against a deployed instance before (and after) an
// upgrade. It exists because a stale spindle.toml put the live instance into
// the restricted boot on 2026-11-22, and nothing could have caught it before
// the deploy.
//
// The command NEVER writes and never repairs. It reads config (the caller does
// that through internal/config's own strict path) and it samples the bucket:
// LIST is allowed here because a doctor command is an explicit diagnostic —
// never on a request path (AGENTS law: no LIST on a hot path) — and every LIST
// is bounded by a per-family sample limit, a per-family count cap and a total
// decode budget. The report states "sampled M of N" rather than pretending the
// scan is exhaustive.
//
// # Concurrency
//
// Run is single-goroutine. It holds no locks and no cache: each store call is
// standalone, so the scan is safe against a live instance whose writers are
// still committing (a key listed and then deleted is simply skipped; there is
// no cross-call invariant to violate). The context cancels every call.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/crueber/spindle/internal/store"
)

// Check names carried by findings. They are stable strings (the --json
// contract) and each names the class of problem, not a subsystem.
const (
	CheckConfig     = "config"          // a validation rule failed (config.Validate)
	CheckConfigKey  = "config.keys"     // an unknown/removed/undecodable key
	CheckConfigLoad = "config.load"     // the config could not be parsed at all
	CheckDecode     = "bucket.decode"   // an object does not decode with its family codec
	CheckVersion    = "bucket.version"  // schema version missing or unknown
	CheckRequired   = "bucket.required" // a required field is empty/invalid
	CheckSize       = "bucket.size"     // size outlier for the family
	CheckArtifact   = "orphan.artifact" // artifact whose run record is gone
	CheckQueue      = "orphan.queue"    // global queue row naming a missing job
	CheckReport     = "orphan.report"   // report index pointer with no record
	CheckFork       = "orphan.fork"     // fork child/parent pointers disagree
	CheckManifest   = "repo.manifest"   // refs exist but the repo has no manifest
)

// Defaults for the bounded scan. They are deliberately generous enough for a
// real instance and small enough that the command stays a diagnostic.
const (
	DefaultLimit  = 200  // objects decoded per family; repos sampled
	DefaultRepos  = 200  // repositories sampled (bounds per-repo families)
	DefaultBudget = 2000 // total decoded objects across every family

	// maxFamilyCount is the hard cap on how many object keys one family counts.
	// N is exact only up to this cap; a family that reaches it reports
	// truncated=true and the text output says "N+ (cap reached)".
	maxFamilyCount = 20000
)

// Options bounds one doctor run.
type Options struct {
	Limit  int // objects decoded per family, and orphan rows per check
	Repos  int // repositories sampled from repos/
	Budget int // total decoded objects across all families
}

// withDefaults normalizes non-positive values to the defaults. The CLI rejects
// negatives before calling; tests may pass zero for "defaults".
func (o Options) withDefaults() Options {
	if o.Limit <= 0 {
		o.Limit = DefaultLimit
	}
	if o.Repos <= 0 {
		o.Repos = DefaultRepos
	}
	if o.Budget <= 0 {
		o.Budget = DefaultBudget
	}
	return o
}

// Finding is one line of the report: the check that found it, the exact bucket
// key path (or config key path) and a short human reason.
type Finding struct {
	Check  string `json:"check"`
	Key    string `json:"key"`
	Reason string `json:"reason"`
}

// FamilyReport states the honesty bound of one family sample.
type FamilyReport struct {
	Family    string `json:"family"`
	Sampled   int    `json:"sampled"`   // objects decoded
	Total     int    `json:"total"`     // objects found in the bounded LIST
	Truncated bool   `json:"truncated"` // the count cap was reached; Total is a floor
	Findings  int    `json:"findings"`
}

// Summary is the machine-readable count block.
type Summary struct {
	Findings        int  `json:"findings"`
	Clean           bool `json:"clean"`
	Limit           int  `json:"limit"`
	Budget          int  `json:"budget"`
	Decoded         int  `json:"decoded"`
	BudgetExhausted bool `json:"budget_exhausted"`
	ReposSampled    int  `json:"repos_sampled"`
	ReposTotal      int  `json:"repos_total"`
	ReposTruncated  bool `json:"repos_truncated"`
}

// Report is the whole doctor answer. It is the stable --json document
// (docs/go/11_config_cli.md §6.7).
type Report struct {
	Version  int            `json:"version"`
	Findings []Finding      `json:"findings"`
	Summary  Summary        `json:"summary"`
	Families []FamilyReport `json:"families"`
}

// Clean reports whether the run found nothing.
func (r *Report) Clean() bool { return r == nil || len(r.Findings) == 0 }

// Add appends findings and keeps Summary.Findings in step.
func (r *Report) Add(fs ...Finding) {
	r.Findings = append(r.Findings, fs...)
	r.Summary.Findings = len(r.Findings)
	r.Summary.Clean = len(r.Findings) == 0
}

// NewReport returns an empty report with the bound recorded.
func NewReport(opts Options) *Report {
	opts = opts.withDefaults()
	return &Report{
		Version:  1,
		Findings: []Finding{},      // JSON [] rather than null (stable shape)
		Families: []FamilyReport{}, // same, before any family is scanned
		Summary:  Summary{Clean: true, Limit: opts.Limit, Budget: opts.Budget},
	}
}

// errStop is the sentinel a List callback returns to stop a bounded walk. The
// store propagates callback errors verbatim, so it is safe to identify.
var errStop = errors.New("doctor: stop listing")

// family is one bucket family the doctor samples. Repo-relative and
// bucket-root prefixes are both supported because the collaboration families
// split across them (per-repo records vs the instance queue/runners).
type family struct {
	name         string
	maxSize      int64    // size outlier threshold; 0 disables the check
	repoPrefixes []string // listed inside every sampled repo
	repoMatch    func(string) bool
	repoExact    []string // exact repo-relative keys (singleton documents)
	rootPrefixes []string // listed once at bucket root
	rootMatch    func(string) bool
	decode       func(key string, raw []byte) *Problem
}

// Problem is one decoder verdict. The zero value is success.
type Problem struct {
	Check  string
	Reason string
}

func problem(check, format string, args ...any) *Problem {
	return &Problem{Check: check, Reason: fmt.Sprintf(format, args...)}
}

// scanner walks the bucket once.
type scanner struct {
	st              store.ObjectStore
	opts            Options
	report          *Report
	families        map[string]*FamilyReport
	decoded         int
	budgetExhausted bool
	repos           []string
	repoTotal       int
	repoTruncated   bool
}

// Run executes every bucket check. The config checks are produced separately
// (ConfigFindings) by the caller, which owns the config ladder; Run never reads
// config and never writes anything.
func Run(ctx context.Context, st store.ObjectStore, opts Options) (*Report, error) {
	opts = opts.withDefaults()
	s := &scanner{st: st, opts: opts, report: NewReport(opts), families: map[string]*FamilyReport{}}
	if st == nil {
		return nil, errors.New("doctor: nil store")
	}
	if err := s.enumerateRepos(ctx); err != nil {
		return nil, err
	}
	s.report.Summary.ReposSampled = len(s.repos)
	s.report.Summary.ReposTotal = s.repoTotal
	s.report.Summary.ReposTruncated = s.repoTruncated
	for _, f := range families() {
		if err := s.scanFamily(ctx, f); err != nil {
			return nil, err
		}
	}
	if err := s.checkOrphans(ctx); err != nil {
		return nil, err
	}
	if err := s.checkManifests(ctx); err != nil {
		return nil, err
	}
	s.report.Summary.Decoded = s.decoded
	s.report.Summary.BudgetExhausted = s.budgetExhausted
	s.report.Add() // normalize Summary.Findings/Clean
	for _, name := range sortedFamilyNames(s.families) {
		s.report.Families = append(s.report.Families, *s.families[name])
	}
	return s.report, nil
}

// enumerateRepos walks repos/<owner>/ via delimiter listings (never a flat
// LIST over every object under repos/, which would stream packs). Bounded by
// opts.Repos samples and maxFamilyCount counted repositories.
func (s *scanner) enumerateRepos(ctx context.Context) error {
	var owners []string
	err := s.st.ListPrefixes(ctx, "repos/", func(p string) error {
		owners = append(owners, p)
		if len(owners) >= maxFamilyCount {
			return errStop
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		return fmt.Errorf("doctor: list repo owners: %w", err)
	}
	for _, ownerPrefix := range owners {
		err := s.st.ListPrefixes(ctx, ownerPrefix, func(p string) error {
			s.repoTotal++
			id := strings.TrimSuffix(strings.TrimPrefix(p, "repos/"), "/")
			if len(s.repos) < s.opts.Repos {
				s.repos = append(s.repos, id)
			} else {
				s.repoTruncated = true
			}
			if s.repoTotal >= maxFamilyCount {
				s.repoTruncated = true
				return errStop
			}
			return nil
		})
		if err != nil && !errors.Is(err, errStop) {
			return fmt.Errorf("doctor: list repos under %s: %w", ownerPrefix, err)
		}
	}
	sort.Strings(s.repos)
	return nil
}

// familyReport returns (creating if needed) one family's bound record.
func (s *scanner) familyReport(name string) *FamilyReport {
	fr := s.families[name]
	if fr == nil {
		fr = &FamilyReport{Family: name}
		s.families[name] = fr
	}
	return fr
}

// scanFamily runs the lists and exact-key probes for one family.
func (s *scanner) scanFamily(ctx context.Context, f family) error {
	fr := s.familyReport(f.name)
	for _, prefix := range f.rootPrefixes {
		if err := s.listInto(ctx, f, fr, prefix, f.rootMatch); err != nil {
			return err
		}
	}
	for _, repo := range s.repos {
		for _, rel := range f.repoPrefixes {
			if err := s.listInto(ctx, f, fr, "repos/"+repo+"/"+rel, f.repoMatch); err != nil {
				return err
			}
		}
		for _, rel := range f.repoExact {
			if err := s.exactInto(ctx, f, fr, "repos/"+repo+"/"+rel); err != nil {
				return err
			}
		}
	}
	return nil
}

// listInto counts matching objects under prefix (up to maxFamilyCount) and
// decodes at most the family's remaining sample budget.
func (s *scanner) listInto(ctx context.Context, f family, fr *FamilyReport, prefix string, match func(string) bool) error {
	var keys []string
	err := s.st.List(ctx, prefix, "", func(m store.ObjectMeta) error {
		if match != nil && !match(m.Key) {
			return nil
		}
		if fr.Total >= maxFamilyCount {
			fr.Truncated = true
			return errStop
		}
		fr.Total++
		if fr.Sampled+len(keys) < s.opts.Limit && !s.budgetExhausted {
			keys = append(keys, m.Key)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStop) {
		return fmt.Errorf("doctor: list %s: %w", prefix, err)
	}
	for _, key := range keys {
		if err := s.decodeKey(ctx, f, fr, key); err != nil {
			return err
		}
	}
	return nil
}

// exactInto probes one exact key. Absent is not a finding here (the manifest
// absence is owned by checkManifests); present keys count and decode.
func (s *scanner) exactInto(ctx context.Context, f family, fr *FamilyReport, key string) error {
	if s.budgetExhausted || fr.Sampled >= s.opts.Limit {
		return nil
	}
	if s.decoded >= s.opts.Budget {
		s.budgetExhausted = true
		return nil
	}
	raw, meta, err := store.GetBytes(ctx, s.st, key, store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("doctor: get %s: %w", key, err)
	}
	s.decoded++
	fr.Total++
	fr.Sampled++
	return s.judge(f, fr, key, raw, meta.Size)
}

// decodeKey GETs one listed key and judges it.
func (s *scanner) decodeKey(ctx context.Context, f family, fr *FamilyReport, key string) error {
	if s.decoded >= s.opts.Budget {
		s.budgetExhausted = true
		return nil
	}
	raw, meta, err := store.GetBytes(ctx, s.st, key, store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return nil // listed then deleted: normal on a live instance
		}
		return fmt.Errorf("doctor: get %s: %w", key, err)
	}
	s.decoded++
	fr.Sampled++
	return s.judge(f, fr, key, raw, meta.Size)
}

// judge applies the size bound and the family codec.
func (s *scanner) judge(f family, fr *FamilyReport, key string, raw []byte, size int64) error {
	if f.maxSize > 0 && size > f.maxSize {
		s.finding(fr, CheckSize, key, "size %d bytes exceeds the %d-byte bound for family %s", size, f.maxSize, f.name)
	}
	if f.decode == nil {
		return nil
	}
	if p := f.decode(key, raw); p != nil {
		s.finding(fr, p.Check, key, "%s", p.Reason)
	}
	return nil
}

func (s *scanner) finding(fr *FamilyReport, check, key, format string, args ...any) {
	// A nil family means the finding belongs to an orphan/manifest check; keep
	// the report's per-family counts only for family sampling.
	if fr != nil {
		fr.Findings++
	}
	s.report.Add(Finding{Check: check, Key: key, Reason: fmt.Sprintf(format, args...)})
}

// sortedFamilyNames returns the family names in the documented order.
func sortedFamilyNames(fam map[string]*FamilyReport) []string {
	order := []string{"manifest", "refs", "access", "identity", "collab", "packages", "threads", "moderation"}
	names := make([]string, 0, len(fam))
	seen := map[string]bool{}
	for _, n := range order {
		if _, ok := fam[n]; ok {
			names = append(names, n)
			seen[n] = true
		}
	}
	var rest []string
	for n := range fam {
		if !seen[n] {
			rest = append(rest, n)
		}
	}
	sort.Strings(rest)
	return append(names, rest...)
}

// ---- output -----------------------------------------------------------------

// WriteText prints one line per finding plus the bound and summary lines.
// The human text is not a stable contract; --json is.
func (r *Report) WriteText(w io.Writer) error {
	for _, f := range r.Findings {
		if _, err := fmt.Fprintf(w, "%-18s %-44s %s\n", f.Check, f.Key, f.Reason); err != nil {
			return err
		}
	}
	for _, fam := range r.Families {
		total := fmt.Sprintf("%d", fam.Total)
		if fam.Truncated {
			total = fmt.Sprintf("%d+ (cap reached)", fam.Total)
		}
		if _, err := fmt.Fprintf(w, "family %s: sampled %d of %s objects (decoded %d)\n",
			fam.Family, fam.Sampled, total, fam.Sampled); err != nil {
			return err
		}
	}
	repos := fmt.Sprintf("%d", r.Summary.ReposTotal)
	if r.Summary.ReposTruncated {
		repos = fmt.Sprintf("%d+ (cap reached)", r.Summary.ReposTotal)
	}
	_, err := fmt.Fprintf(w, "repos: sampled %d of %s\n", r.Summary.ReposSampled, repos)
	if err != nil {
		return err
	}
	if r.Summary.BudgetExhausted {
		if _, err := fmt.Fprintf(w, "budget: %d objects decoded (budget reached; later families are not fully sampled)\n", r.Summary.Decoded); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(w, "doctor: %d findings, %s\n", r.Summary.Findings, cleanWord(r.Summary.Findings))
	return err
}

func cleanWord(n int) string {
	if n == 0 {
		return "clean"
	}
	return "not clean"
}
