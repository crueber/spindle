// usage.go — the stored usage counter (docs/features/14 §5, quota + retention).
// One small CAS JSON object per repository records the bytes the registry
// holds, so the Packages page, the quota check at the push commit point and
// the Maintenance card never enumerate the bucket (law 4: no LIST on a request
// path).
//
//	repos/<o>/<r>/packages/usage.json   per-repository counter (CAS)
//	packages/usage.json                 instance index: per-repo bytes + total
//
// The per-repository counter is adjusted on blob finalize, manifest put and
// version deletion, and reconciled to an absolute reading by every maintainer
// sweep (the only place the registry lists itself). The instance index is
// written ONLY by the maintainer pass: a second instance-level CAS on every
// layer upload would be a hot-path round trip nobody asked for, so the
// Maintenance card says "as of the last pass" and stays honest.
package packages

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/crueber/spindle/internal/config"
	"github.com/crueber/spindle/internal/store"
)

// usageStateVersion is the on-bucket shape version (append-only: a reader
// tolerates an unknown version by reading the fields it knows).
const usageStateVersion = 1

// UsageKey is one repository's counter object.
func UsageKey(owner, repo string) string { return RepoBase(owner, repo) + "/usage.json" }

// InstanceUsageKey is the instance index at the bucket root (spindle-only
// prefix; it never collides with a Rust-era key).
func InstanceUsageKey() string { return "packages/usage.json" }

// Usage is the stored counter document. Bytes is blob bytes + manifest bytes —
// the physical registry footprint a quota bounds; the counts are display
// facts.
type Usage struct {
	Version       int    `json:"version"`
	Bytes         int64  `json:"bytes"`
	BlobBytes     int64  `json:"blob_bytes"`
	ManifestBytes int64  `json:"manifest_bytes"`
	Blobs         int64  `json:"blobs"`
	Manifests     int64  `json:"manifests"`
	UpdatedAt     string `json:"updated_at"`
}

// UsageInfo is the read projection the page, the JSON API and the quota check
// share: the counter plus the resolved quota (0 = unlimited).
type UsageInfo struct {
	Bytes         int64  `json:"bytes"`
	BlobBytes     int64  `json:"blob_bytes"`
	ManifestBytes int64  `json:"manifest_bytes"`
	Blobs         int64  `json:"blobs"`
	Manifests     int64  `json:"manifests"`
	QuotaBytes    int64  `json:"quota_bytes"` // 0 = unlimited
	UpdatedAt     string `json:"updated_at"`
}

// Limited reports whether a quota is in force.
func (u *UsageInfo) Limited() bool { return u != nil && u.QuotaBytes > 0 }

// OverQuota reports whether the stored bytes exceed a configured quota. The
// commit point refuses a push that would cross the line; this is the state
// an operator lowering a quota can produce, and the page renders it in words.
func (u *UsageInfo) OverQuota() bool { return u.Limited() && u.Bytes > u.QuotaBytes }

// Percent is the quota consumed, clamped to 100 for a meter's width (0 when
// unlimited). It is a decoration: the state is always stated in text too.
func (u *UsageInfo) Percent() int {
	if !u.Limited() || u.Bytes <= 0 {
		return 0
	}
	p := int(u.Bytes * 100 / u.QuotaBytes)
	if p > 100 {
		return 100
	}
	return p
}

// Remaining is the bytes left under the quota (0 when unlimited or full).
func (u *UsageInfo) Remaining() int64 {
	if !u.Limited() {
		return 0
	}
	if u.Bytes >= u.QuotaBytes {
		return 0
	}
	return u.QuotaBytes - u.Bytes
}

// readUsage reads one repository's counter. A missing object is a zero
// reading with an empty version (the first write creates it).
func (s *Service) readUsage(ctx context.Context, owner, repo string) (Usage, store.Version, error) {
	raw, meta, err := store.GetBytes(ctx, s.Store, UsageKey(owner, repo), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return Usage{Version: usageStateVersion}, "", nil
		}
		return Usage{}, "", err
	}
	u := Usage{Version: usageStateVersion}
	if len(raw) > 0 {
		if uerr := json.Unmarshal(raw, &u); uerr != nil {
			// A corrupt counter must not brick the registry: treat it as a
			// zero reading — the next sweep reconciles the absolute number
			// and reports the drift.
			s.logf("packages usage %s is corrupt (%v); treating it as zero until the next sweep reconciles it",
				UsageKey(owner, repo), uerr)
			u = Usage{Version: usageStateVersion}
		}
	}
	if u.Version == 0 {
		u.Version = usageStateVersion
	}
	return u, meta.Version, nil
}

// adjustUsage applies a delta to the counter with one CAS (create when absent,
// update against the version we read). Contention is a concurrent layer or
// manifest write; four attempts is generous for a push's parallelism, and a
// lost delta is not data loss — the maintainer sweep reconciles the absolute
// reading and reports the drift, so this never fails a request whose object
// already committed.
func (s *Service) adjustUsage(ctx context.Context, owner, repo string, delta Usage) {
	const attempts = 4
	for i := 0; i < attempts; i++ {
		cur, ver, err := s.readUsage(ctx, owner, repo)
		if err != nil {
			s.logf("packages usage %s: read failed: %v", UsageKey(owner, repo), err)
			return
		}
		cur.Bytes += delta.Bytes
		cur.BlobBytes += delta.BlobBytes
		cur.ManifestBytes += delta.ManifestBytes
		cur.Blobs += delta.Blobs
		cur.Manifests += delta.Manifests
		clampNonNegative(&cur)
		cur.Version = usageStateVersion
		cur.UpdatedAt = s.now().Format(time.RFC3339)
		perr := s.putUsage(ctx, owner, repo, cur, ver)
		if perr == nil {
			return
		}
		if !store.IsPreconditionFailed(perr) {
			s.logf("packages usage %s: write failed: %v", UsageKey(owner, repo), perr)
			return
		}
	}
	s.logf("packages usage %s: counter changed concurrently; the next sweep reconciles it", UsageKey(owner, repo))
}

// clampNonNegative keeps a delta that raced a reconcile from going below zero
// (a display bug, not a data bug).
func clampNonNegative(u *Usage) {
	if u.Bytes < 0 {
		u.Bytes = 0
	}
	if u.BlobBytes < 0 {
		u.BlobBytes = 0
	}
	if u.ManifestBytes < 0 {
		u.ManifestBytes = 0
	}
	if u.Blobs < 0 {
		u.Blobs = 0
	}
	if u.Manifests < 0 {
		u.Manifests = 0
	}
}

// setUsage writes an absolute reading (the maintainer reconcile), CAS'd so a
// push committing a layer while the sweep finishes its walk is retried rather
// than clobbered.
func (s *Service) setUsage(ctx context.Context, owner, repo string, u Usage) error {
	const attempts = 4
	for i := 0; i < attempts; i++ {
		_, ver, err := s.readUsage(ctx, owner, repo)
		if err != nil {
			return err
		}
		u.Version = usageStateVersion
		u.UpdatedAt = s.now().Format(time.RFC3339)
		perr := s.putUsage(ctx, owner, repo, u, ver)
		if perr == nil {
			return nil
		}
		if !store.IsPreconditionFailed(perr) {
			return perr
		}
	}
	return nil
}

// adjustUsageFrom applies delta using a counter reading the caller already
// has (the finalize path reads it for the quota check), falling back to a
// fresh read-and-CAS when the fast attempt loses the version race. It keeps a
// layer upload at one counter read and one write in the uncontended case.
func (s *Service) adjustUsageFrom(ctx context.Context, owner, repo string, cur Usage, ver store.Version, delta Usage) {
	next := cur
	next.Bytes += delta.Bytes
	next.BlobBytes += delta.BlobBytes
	next.ManifestBytes += delta.ManifestBytes
	next.Blobs += delta.Blobs
	next.Manifests += delta.Manifests
	clampNonNegative(&next)
	next.Version = usageStateVersion
	next.UpdatedAt = s.now().Format(time.RFC3339)
	perr := s.putUsage(ctx, owner, repo, next, ver)
	if perr == nil {
		return
	}
	if store.IsPreconditionFailed(perr) {
		s.adjustUsage(ctx, owner, repo, delta)
		return
	}
	s.logf("packages usage %s: write failed: %v", UsageKey(owner, repo), perr)
}

func (s *Service) putUsage(ctx context.Context, owner, repo string, u Usage, ver store.Version) error {
	raw, err := json.Marshal(u)
	if err != nil {
		return err
	}
	opts := store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}
	if ver != "" {
		opts = store.PutOptions{Mode: store.PutUpdate, IfVersion: ver, ContentType: "application/json"}
	}
	_, err = s.Store.Put(ctx, UsageKey(owner, repo), store.PutBody{Bytes: raw}, opts)
	return err
}

// policyFor resolves the effective policy for one repository (instance
// default when no projection is wired: unlimited, no retention — the
// compatible behavior for tests and un-wired builds).
func (s *Service) policyFor(ctx context.Context, owner, repo string) config.PackagesPolicy {
	if s.Policy == nil {
		return config.PackagesPolicy{}
	}
	return s.Policy(ctx, owner, repo)
}

// Usage reads the counter and the resolved quota (the page/API projection).
// One GET plus the policy projection: a page path, never a pull.
func (s *Service) Usage(ctx context.Context, owner, repo string) (*UsageInfo, *Error) {
	if err := validateRepo(owner, repo); err != nil {
		return nil, err
	}
	u, _, err := s.readUsage(ctx, owner, repo)
	if err != nil {
		return nil, mapStoreErr(err)
	}
	pol := s.policyFor(ctx, owner, repo)
	return &UsageInfo{
		Bytes:         u.Bytes,
		BlobBytes:     u.BlobBytes,
		ManifestBytes: u.ManifestBytes,
		Blobs:         u.Blobs,
		Manifests:     u.Manifests,
		QuotaBytes:    pol.QuotaBytes,
		UpdatedAt:     u.UpdatedAt,
	}, nil
}

// InstanceUsage is the stored instance index: every registry's last
// reconciled byte count and their sum. A missing object reads as zero (never
// an error the card has to explain).
type InstanceUsage struct {
	Version    int              `json:"version"`
	UpdatedAt  string           `json:"updated_at"`
	TotalBytes int64            `json:"total_bytes"`
	Repos      map[string]int64 `json:"repos"`
}

// RepoUsage is one row of the instance index.
type RepoUsage struct {
	Repo  string `json:"repo"`
	Bytes int64  `json:"bytes"`
}

// Largest returns the n largest registries (bytes desc, then name for a
// stable order).
func (u *InstanceUsage) Largest(n int) []RepoUsage {
	if u == nil {
		return nil
	}
	rows := make([]RepoUsage, 0, len(u.Repos))
	for id, b := range u.Repos {
		rows = append(rows, RepoUsage{Repo: id, Bytes: b})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Bytes != rows[j].Bytes {
			return rows[i].Bytes > rows[j].Bytes
		}
		return rows[i].Repo < rows[j].Repo
	})
	if n > 0 && len(rows) > n {
		rows = rows[:n]
	}
	return rows
}

// InstanceUsage reads the instance index (one GET; the Maintenance card).
func (s *Service) InstanceUsage(ctx context.Context) (*InstanceUsage, error) {
	raw, _, err := store.GetBytes(ctx, s.Store, InstanceUsageKey(), store.GetOptions{})
	if err != nil {
		if store.IsNotFound(err) {
			return &InstanceUsage{Version: usageStateVersion, Repos: map[string]int64{}}, nil
		}
		return nil, mapStoreErr(err)
	}
	out := InstanceUsage{Version: usageStateVersion, Repos: map[string]int64{}}
	if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return nil, errInternal(uerr)
	}
	if out.Repos == nil {
		out.Repos = map[string]int64{}
	}
	if out.Version == 0 {
		out.Version = usageStateVersion
	}
	return &out, nil
}

// pruneInstanceUsage drops instance-index rows for registries the instance
// pass did not enumerate (a moved or deleted repository). Called only by
// SweepAll on an applying pass: it is the one writer that sees the whole set.
func (s *Service) pruneInstanceUsage(ctx context.Context, known map[string]bool) {
	cur, err := s.InstanceUsage(ctx)
	if err != nil {
		s.logf("packages instance usage: prune read failed: %v", err)
		return
	}
	stale := false
	for id := range cur.Repos {
		if !known[id] {
			stale = true
		}
	}
	if !stale {
		return
	}
	const attempts = 4
	for i := 0; i < attempts; i++ {
		raw, meta, gerr := store.GetBytes(ctx, s.Store, InstanceUsageKey(), store.GetOptions{})
		if gerr != nil {
			if store.IsNotFound(gerr) {
				return
			}
			s.logf("packages instance usage: prune read failed: %v", gerr)
			return
		}
		st := InstanceUsage{Version: usageStateVersion, Repos: map[string]int64{}}
		if uerr := json.Unmarshal(raw, &st); uerr != nil {
			return
		}
		if st.Repos == nil {
			st.Repos = map[string]int64{}
		}
		for id := range st.Repos {
			if !known[id] {
				delete(st.Repos, id)
			}
		}
		st.Version = usageStateVersion
		st.TotalBytes = 0
		for _, b := range st.Repos {
			st.TotalBytes += b
		}
		st.UpdatedAt = s.now().Format(time.RFC3339)
		body, merr := json.Marshal(st)
		if merr != nil {
			return
		}
		if _, perr := s.Store.Put(ctx, InstanceUsageKey(), store.PutBody{Bytes: body}, store.PutOptions{
			Mode: store.PutUpdate, IfVersion: meta.Version, ContentType: "application/json",
		}); perr != nil {
			if store.IsPreconditionFailed(perr) {
				continue
			}
			s.logf("packages instance usage: prune write failed: %v", perr)
			return
		}
		return
	}
}

// noteInstanceUsage records one repository's reconciled bytes in the instance
// index (maintainer pass only). Zero bytes drops the entry, so a registry that
// no longer holds objects stops being counted.
func (s *Service) noteInstanceUsage(ctx context.Context, owner, repo string, bytes int64) {
	id := owner + "/" + repo
	const attempts = 4
	for i := 0; i < attempts; i++ {
		raw, meta, err := store.GetBytes(ctx, s.Store, InstanceUsageKey(), store.GetOptions{})
		if err != nil && !store.IsNotFound(err) {
			s.logf("packages instance usage: read failed: %v", err)
			return
		}
		st := InstanceUsage{Version: usageStateVersion, Repos: map[string]int64{}}
		if len(raw) > 0 {
			if uerr := json.Unmarshal(raw, &st); uerr != nil {
				// Rebuild rather than refuse: the index is bookkeeping, and
				// this pass knows one true row of it.
				st = InstanceUsage{Version: usageStateVersion, Repos: map[string]int64{}}
			}
		}
		if st.Repos == nil {
			st.Repos = map[string]int64{}
		}
		if bytes <= 0 {
			delete(st.Repos, id)
		} else {
			st.Repos[id] = bytes
		}
		st.Version = usageStateVersion
		st.TotalBytes = 0
		for _, b := range st.Repos {
			st.TotalBytes += b
		}
		st.UpdatedAt = s.now().Format(time.RFC3339)
		body, merr := json.Marshal(st)
		if merr != nil {
			s.logf("packages instance usage: encode failed: %v", merr)
			return
		}
		opts := store.PutOptions{Mode: store.PutCreate, ContentType: "application/json"}
		if meta.Version != "" {
			opts = store.PutOptions{Mode: store.PutUpdate, IfVersion: meta.Version, ContentType: "application/json"}
		}
		if _, perr := s.Store.Put(ctx, InstanceUsageKey(), store.PutBody{Bytes: body}, opts); perr != nil {
			if store.IsPreconditionFailed(perr) {
				continue
			}
			s.logf("packages instance usage: write failed: %v", perr)
			return
		}
		return
	}
}
