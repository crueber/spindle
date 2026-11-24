// gcadmin.go — the instance-scope packages GC control (docs/PLAN-ADMIN.md
// §3.5/§8.6, docs/features/14_packages.md §6): a dry run that counts what a
// sweep would reclaim, one applying pass, and the durable last-run summary the
// Administration area's Maintenance page reads.
//
// The unit of work is still the per-repository mark/sweep (gc.go): a registry
// lives under repos/<o>/<r>/packages/registry, so "the instance's packages GC"
// is that same pass over every registry the store holds. Nothing new is
// invented about what is garbage.
//
// # Where the state lives
//
//	packages/gc_state.json   overwrite JSON (version 1), bucket root
//
// The task table is instance memory, so a restart would otherwise lose the
// only answer to "when did GC last run, and what did it reclaim". The summary
// is written by the pass itself (the actor and task id ride along), so the
// page is honest across restarts; a lost object means "last run: not recorded
// on this instance", never a fabricated number.
//
// # The task
//
// The pass runs on the core (repo, kind) task table as the EXISTING
// packages-gc kind — a manual start joins a running pass instead of racing it
// (law 7), and the operator's card attaches to the same narration the
// maintainer's sweeps emit. The instance scope is the reserved task-table repo
// key "instance": no repository is named "instance" (owner names are reserved
// per docs/go/06 §2.2 and a repo key is always "<owner>/<repo>"), so an
// instance pass never shadows or joins a real repository's sweep.
//
// # Listings
//
// Enumerating registries is a delimiter LIST over repos/ — the maintainer's
// shape, never a request path (law 4). The page reads only the stored summary
// and the live task record; neither is a LIST.
package packages

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/crueber/spindle/internal/store"
	"github.com/crueber/spindle/internal/wal"
)

// InstanceScope is the task-table repo key an instance-scope packages-gc pass
// runs under. It cannot collide with a repository id ("<owner>/<repo>").
const InstanceScope = "instance"

// AuditActionPackagesGC is the audit verb a GC trigger writes (the host audit
// family's administrative vocabulary, PLAN-V2 §M.4).
const AuditActionPackagesGC = "packages_gc"

// auditTarget is the host-level target an instance GC names.
const auditTarget = "instance:packages"

// GCStateKey is the durable last-run summary (bucket root, overwrite JSON).
func GCStateKey() string { return "packages/gc_state.json" }

// GCSweep is the aggregate one instance pass reported. A dry run and an
// applying pass share the shape on purpose: the page can put the count and the
// result side by side without redefining "unreferenced".
type GCSweep struct {
	DryRun       bool  `json:"dry_run"`
	Repos        int   `json:"repos"`        // registries that carried objects
	Manifests    int   `json:"manifests"`    // manifest objects scanned (mark)
	Blobs        int   `json:"blobs"`        // blob objects seen (sweep)
	Unreferenced int   `json:"unreferenced"` // blobs a GC would reclaim / did reclaim
	Bytes        int64 `json:"bytes"`        // their bytes
	Uploads      int   `json:"uploads"`      // expired staging uploads
	Tombstones   int   `json:"tombstones"`   // expired abort tombstones
	Failed       int   `json:"failed"`       // registries whose pass returned an error
	// Retention is the tag-retention total across registries (docs/features/14
	// §5): tag pointers pruned and the manifest versions they took with them.
	PrunedTags      int `json:"pruned_tags"`
	PrunedManifests int `json:"pruned_manifests"`
	// UsageDrift is the sum of |counter − walk| the pass repaired (apply) or
	// would repair (dry run): the reconciliation report.
	UsageDrift int64  `json:"usage_drift"`
	At         string `json:"at"` // UTC RFC 3339, the pass's finish
}

// Summary is the one-line human reading (the task's terminal narration).
func (r GCSweep) Summary() string {
	mode := "reclaimed"
	if r.DryRun {
		mode = "would reclaim"
	}
	s := fmt.Sprintf("%s %d blob(s) in %s across %d registr%s (%d manifest(s) scanned)",
		mode, r.Unreferenced, humanBytes(r.Bytes), r.Repos, plural(r.Repos, "y", "ies"), r.Manifests)
	if r.Uploads > 0 || r.Tombstones > 0 {
		s += fmt.Sprintf(", %d expired upload(s) and %d tombstone(s)", r.Uploads, r.Tombstones)
	}
	if r.PrunedTags > 0 {
		s += fmt.Sprintf(", retention pruned %d tag(s) and %d manifest version(s)", r.PrunedTags, r.PrunedManifests)
	}
	if r.UsageDrift != 0 {
		s += fmt.Sprintf(", usage counter repaired %d byte(s) of drift", r.UsageDrift)
	}
	if r.Failed > 0 {
		s += fmt.Sprintf("; %d registr%s failed — the next pass retries them", r.Failed, plural(r.Failed, "y", "ies"))
	}
	return s
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// humanBytes renders a byte count with binary units (the page's spelling).
func humanBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 5; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// GCRun is one recorded pass (the stored summary's row).
type GCRun struct {
	At      string `json:"at"`
	DryRun  bool   `json:"dry_run"`
	Actor   string `json:"actor"`
	TaskID  string `json:"task_id,omitempty"`
	Repos   int    `json:"repos"`
	Deleted int    `json:"unreferenced"`
	Bytes   int64  `json:"bytes"`
	Uploads int    `json:"uploads"`
	// Retention + reconciliation facts (additive; an older stored row reads
	// zero, never a fabricated number).
	PrunedTags      int    `json:"pruned_tags,omitempty"`
	PrunedManifests int    `json:"pruned_manifests,omitempty"`
	UsageDrift      int64  `json:"usage_drift,omitempty"`
	OK              bool   `json:"ok"`
	Error           string `json:"error,omitempty"`
}

// GCState is the stored document: the most recent pass (either mode) and the
// most recent applying pass, so the page can be honest about which number it
// is showing.
type GCState struct {
	Version   int    `json:"version"`
	Last      *GCRun `json:"last,omitempty"`
	LastApply *GCRun `json:"last_apply,omitempty"`
}

// GCState reads the stored summary; absent means no pass was recorded here
// (never an error the page has to explain).
func (s *Service) GCState(ctx context.Context) (*GCState, error) {
	raw, _, err := store.GetBytes(ctx, s.Store, GCStateKey(), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return &GCState{Version: 1}, nil
		}
		return nil, mapStoreErr(err)
	}
	if len(raw) == 0 {
		return &GCState{Version: 1}, nil
	}
	var st GCState
	if uerr := json.Unmarshal(raw, &st); uerr != nil {
		return nil, fmt.Errorf("corrupt GC summary %s: %w", GCStateKey(), uerr)
	}
	if st.Version == 0 {
		st.Version = 1
	}
	return &st, nil
}

// putGCState CAS-overwrites the summary. Contention is two operators pressing
// the button at once; five attempts is generous at human rate.
func (s *Service) putGCState(ctx context.Context, mutate func(*GCState)) error {
	const attempts = 5
	for i := 0; i < attempts; i++ {
		raw, meta, err := store.GetBytes(ctx, s.Store, GCStateKey(), store.GetOptions{})
		if err != nil && !store.IsNotFound(err) {
			return mapStoreErr(err)
		}
		st := GCState{Version: 1}
		if len(raw) > 0 {
			if uerr := json.Unmarshal(raw, &st); uerr != nil {
				return fmt.Errorf("corrupt GC summary %s: %w", GCStateKey(), uerr)
			}
		}
		st.Version = 1
		mutate(&st)
		body, merr := json.Marshal(st)
		if merr != nil {
			return merr
		}
		opts := store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version, ContentType: "application/json"}
		if meta.Version == "" {
			opts.Mode = store.PutCreate
		}
		if _, perr := store.PutBytes(ctx, s.Store, GCStateKey(), body, opts); perr != nil {
			if store.IsPreconditionFailed(perr) {
				continue
			}
			return mapStoreErr(perr)
		}
		return nil
	}
	return fmt.Errorf("GC summary changed concurrently; retry")
}

// GCSweepRequest scopes one instance pass.
type GCSweepRequest struct {
	DryRun bool
	Actor  string
	TaskID string
}

// SweepAll runs one instance pass: enumerate every registry, run the same
// mark/sweep body per repository, and record the aggregate. A failure inside
// one repository fails THAT repository (counted, named in the narration) and
// the pass continues — a single unreadable registry must not hide the rest.
func (s *Service) SweepAll(ctx context.Context, req GCSweepRequest, log TaskLogger) (GCSweep, error) {
	out := GCSweep{DryRun: req.DryRun}
	repos, err := s.registryRepos(ctx)
	if err != nil {
		s.recordSweep(ctx, req, out, err)
		return out, mapStoreErr(err)
	}
	// A manual dry run should not be silently served from the grace window of
	// a pass that just ran: prune only (no due-gate change on a count).
	s.pruneRecent(s.now())
	for i, id := range repos {
		if ctx.Err() != nil {
			s.recordSweep(ctx, req, out, ctx.Err())
			return out, ctx.Err()
		}
		if log != nil {
			log.Progress("repositories", uint64(i), uint64(len(repos)), "registry")
		}
		var res GCResult
		var rerr error
		if req.DryRun {
			res, rerr = s.PlanGC(ctx, id[0], id[1], log)
		} else {
			// The applying pass is the maintainer's own GC verb, so a manual
			// run and a scheduled sweep cannot diverge (and the due-gate clock
			// moves exactly as the maintainer's would).
			res, rerr = s.GC(ctx, id[0], id[1], log)
		}
		out.Repos++
		out.Manifests += res.Manifests
		out.Blobs += res.Blobs
		out.Unreferenced += res.Deleted
		out.Bytes += res.BytesFreed
		out.Uploads += res.Uploads
		out.Tombstones += res.Tombstones
		if res.Retention != nil {
			out.PrunedTags += res.Retention.PrunedTags
			out.PrunedManifests += res.Retention.Manifests
		}
		if res.UsageDrift > 0 {
			out.UsageDrift += res.UsageDrift
		} else {
			out.UsageDrift -= res.UsageDrift
		}
		if rerr != nil {
			out.Failed++
			if log != nil {
				log.Notice("packages-gc " + id[0] + "/" + id[1] + ": " + rerr.Error())
			}
			s.logf("packages-gc instance pass: %s/%s failed: %v", id[0], id[1], rerr)
		}
	}
	if log != nil {
		log.Progress("repositories", uint64(len(repos)), uint64(len(repos)), "registry")
	}
	// The instance pass is the one place that knows the whole registry set, so
	// it drops index rows for registries that no longer exist (a moved or
	// deleted repository). Per-repository passes cannot see the other rows, so
	// this is the only writer allowed to forget one.
	if !req.DryRun {
		known := make(map[string]bool, len(repos))
		for _, id := range repos {
			known[id[0]+"/"+id[1]] = true
		}
		s.pruneInstanceUsage(ctx, known)
	}
	out.At = s.now().Format(time.RFC3339)
	// A partial failure is a failed pass with work done: the task record and the
	// stored summary both say so, and the next pass retries only the failures.
	var passErr error
	if out.Failed > 0 {
		passErr = fmt.Errorf("%d registr%s could not be swept — the work that succeeded is committed; the next pass retries the rest",
			out.Failed, plural(out.Failed, "y", "ies"))
	}
	s.recordSweep(ctx, req, out, passErr)
	if log != nil {
		log.Notice("packages-gc instance: " + out.Summary())
	}
	return out, passErr
}

// recordSweep stores the run and never fails the pass for it (the summary is
// bookkeeping; the deletions already committed). A nil err records a completed
// pass, a non-nil one records the failure so the page can say so.
func (s *Service) recordSweep(ctx context.Context, req GCSweepRequest, out GCSweep, passErr error) {
	if out.At == "" {
		out.At = s.now().Format(time.RFC3339)
	}
	run := &GCRun{
		At: out.At, DryRun: out.DryRun, Actor: req.Actor, TaskID: req.TaskID,
		Repos: out.Repos, Deleted: out.Unreferenced, Bytes: out.Bytes, Uploads: out.Uploads,
		PrunedTags: out.PrunedTags, PrunedManifests: out.PrunedManifests, UsageDrift: out.UsageDrift,
		OK: passErr == nil,
	}
	if passErr != nil {
		run.Error = passErr.Error()
		if errors.Is(passErr, context.Canceled) {
			run.Error = "cancelled (a cancelled pass is safe; the next one retries)"
		}
	}
	if err := s.putGCState(ctx, func(st *GCState) {
		st.Last = run
		if !out.DryRun {
			cp := *run
			st.LastApply = &cp
		}
	}); err != nil {
		s.logf("packages-gc summary write failed: %v", err)
	}
}

// registryRepos enumerates the repositories that hold registry objects. The
// delimiter listings are the maintainer pass's LISTs (law 4: never a request
// path); a repository with no packages prefix is skipped without touching its
// keys.
func (s *Service) registryRepos(ctx context.Context) ([][2]string, error) {
	if s.Repos != nil {
		return s.Repos(ctx)
	}
	var owners []string
	if err := s.Store.ListPrefixes(ctx, "repos/", func(p string) error {
		if o := strings.TrimSuffix(strings.TrimPrefix(p, "repos/"), "/"); o != "" {
			owners = append(owners, o)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	var out [][2]string
	for _, owner := range owners {
		var repos []string
		if err := s.Store.ListPrefixes(ctx, "repos/"+owner+"/", func(p string) error {
			if name := strings.TrimSuffix(strings.TrimPrefix(p, "repos/"+owner+"/"), "/"); name != "" {
				repos = append(repos, name)
			}
			return nil
		}); err != nil {
			return nil, err
		}
		for _, repo := range repos {
			ok, herr := s.hasRegistry(ctx, owner, repo)
			if herr != nil {
				return nil, herr
			}
			if ok {
				out = append(out, [2]string{owner, repo})
			}
		}
	}
	return out, nil
}

// errFirstObject ends a delimiter probe after its first object.
var errFirstObject = errors.New("first object seen")

// hasRegistry probes one repository's packages prefix without walking it.
func (s *Service) hasRegistry(ctx context.Context, owner, repo string) (bool, error) {
	found := false
	err := s.Store.ListPrefixes(ctx, RepoBase(owner, repo)+"/", func(string) error {
		found = true
		return errFirstObject
	})
	if err != nil && !errors.Is(err, errFirstObject) {
		return false, err
	}
	return found, nil
}

// --- the manual trigger (Seam 5: the same task table every op uses) -----------

// TaskRunner is the (repo, kind) task table this package drives (satisfied by
// *wal.TaskTable at composition; nil = no manual trigger, the maintainer's own
// sweeps still run).
type TaskRunner interface {
	Run(ctx context.Context, repo, kind string, params map[string]string, fn func(ctx context.Context, task *wal.Task) error) (*wal.TaskRecord, error)
	Running(repo, kind string) *wal.TaskRecord
}

// GCTask is the started (or joined) pass the admin card renders.
type GCTask struct {
	ID     string
	Kind   string
	DryRun bool
	Joined bool
}

// AuditFunc records one host-audit entry (composition binds
// moderation.Service.RecordAdmin).
type AuditFunc func(ctx context.Context, actor, action, target, detail string) error

// StartGC starts (or joins) an instance pass on the task table and writes the
// trigger's audit record. It answers the task id before the pass finishes —
// the page renders the card and attaches to the stream (law 7: the operator
// sees progress, never a silent spinner).
func (s *Service) StartGC(ctx context.Context, tasks TaskRunner, actor string, dryRun bool) (GCTask, error) {
	if tasks == nil {
		return GCTask{}, fmt.Errorf("packages GC needs the task table; it is not configured on this build")
	}
	if running := tasks.Running(InstanceScope, TaskKindGC); running != nil {
		s.auditGC(ctx, actor, dryRun, running.ID, "joined the running pass")
		return GCTask{ID: running.ID, Kind: running.Kind, DryRun: dryRun, Joined: true}, nil
	}
	params := map[string]string{"scope": InstanceScope, "dry_run": boolDigit(dryRun)}
	started := make(chan *wal.TaskRecord, 1)
	finished := make(chan error, 1)
	go func() {
		_, err := tasks.Run(context.WithoutCancel(ctx), InstanceScope, TaskKindGC, params,
			func(tctx context.Context, task *wal.Task) error {
				cp := task.Record()
				select {
				case started <- &cp:
				default:
				}
				_, serr := s.SweepAll(tctx, GCSweepRequest{DryRun: dryRun, Actor: actor, TaskID: cp.ID}, task)
				return serr
			})
		finished <- err
	}()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	var id string
	select {
	case rec := <-started:
		id = rec.ID
	case err := <-finished:
		// A draining table refused the start, or the pass finished inside the
		// scheduling window — answer the join hint, then the error.
		if running := tasks.Running(InstanceScope, TaskKindGC); running != nil {
			id = running.ID
		} else if err != nil {
			return GCTask{}, err
		}
	case <-timer.C:
		// The table joined an existing pass (its leader owns the record): the
		// documented join hint answers the id without waiting for the move.
		if running := tasks.Running(InstanceScope, TaskKindGC); running != nil {
			id = running.ID
		}
	case <-ctx.Done():
		return GCTask{}, ctx.Err()
	}
	if id == "" {
		return GCTask{}, fmt.Errorf("packages GC did not start; retry")
	}
	s.auditGC(ctx, actor, dryRun, id, "started a new pass")
	return GCTask{ID: id, Kind: TaskKindGC, DryRun: dryRun}, nil
}

func boolDigit(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// auditGC writes the trigger record. A failed audit write never fails the
// start (audit-after-commit): it is logged, and the pass is already running.
func (s *Service) auditGC(ctx context.Context, actor string, dryRun bool, taskID, how string) {
	detail := "mode=dry-run;task=" + taskID + ";" + how
	if !dryRun {
		detail = "mode=apply;task=" + taskID + ";" + how
	}
	if s.Audit == nil {
		return
	}
	if err := s.Audit(ctx, actor, AuditActionPackagesGC, auditTarget, detail); err != nil {
		s.logf("packages-gc audit write failed: %v", err)
	}
}

// GCStatus is the page projection: the live pass (when one runs) plus the
// stored last runs.
type GCStatus struct {
	Running *wal.TaskRecord
	State   *GCState
}

// GCStatus reads what the page shows: the running instance pass (task-table
// memory) and the durable summary. Reads only — no LIST.
func (s *Service) GCStatus(ctx context.Context, tasks TaskRunner) (GCStatus, error) {
	var st GCStatus
	if tasks != nil {
		st.Running = tasks.Running(InstanceScope, TaskKindGC)
	}
	state, err := s.GCState(ctx)
	if err != nil {
		return st, err
	}
	st.State = state
	return st, nil
}

func (s *Service) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}
