package releases

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/crueber/spindle/internal/store"
)

// This file owns the per-asset download counters (docs/PLAN-V1.md WP3.2):
// one CAS'd object per release (DownloadCountsKey) bumped from the asset byte
// route, best-effort — a counter never fails a download and never touches a
// push path.
//
// ### Concurrency
//
// Hazard: concurrent downloads of two assets on the same release both CAS the
// one counts object (lost update). Avoidance: the canonical read-modify-write
// loop (casUpdate) — the 412 loser re-reads and applies its single-field
// increment again; human-rate traffic means retries are ~never taken. The
// increment is best-effort (bounded attempts, errors swallowed by the byte
// route) so no download can be delayed into failure by counter contention.

// DownloadCounts is the counts object body.
type DownloadCounts struct {
	Counts map[string]int64 `json:"counts"`
}

// CountFor returns one asset's count (0 when absent).
func (d *DownloadCounts) CountFor(name string) int64 {
	if d == nil {
		return 0
	}
	return d.Counts[name]
}

// parseCounts decodes a counts object (corrupt counts read as empty: a broken
// counter must never break a release page).
func parseCounts(raw []byte) *DownloadCounts {
	var d DownloadCounts
	if len(raw) == 0 || json.Unmarshal(raw, &d) != nil {
		return &DownloadCounts{Counts: map[string]int64{}}
	}
	if d.Counts == nil {
		d.Counts = map[string]int64{}
	}
	return &d
}

// AssetDownloads reads the release's counter object (never an error surface:
// a missing or unreadable counter reads as zero counts).
func (s *Service) AssetDownloads(ctx context.Context, owner, repo, tag string) map[string]int64 {
	raw, _, err := s.getJSON(ctx, DownloadCountsKey(owner, repo, tag))
	if err != nil {
		return map[string]int64{}
	}
	return parseCounts(raw).Counts
}

// BumpDownload increments one asset's counter (one CAS loop, best-effort).
// name MUST already be a validated asset name (the byte route validates before
// calling). Errors are returned for tests; the byte route ignores them.
func (s *Service) BumpDownload(ctx context.Context, owner, repo, tag, name string) error {
	key := DownloadCountsKey(owner, repo, tag)
	_, err := s.casUpdate(ctx, key, 6, func(cur []byte, ver store.Version) ([]byte, bool, error) {
		d := parseCounts(cur)
		if d.Counts == nil {
			d.Counts = map[string]int64{}
		}
		d.Counts[name]++
		raw, jerr := json.Marshal(d)
		if jerr != nil {
			return nil, false, fmt.Errorf("%w: counts: %v", ErrCorrupt, jerr)
		}
		return raw, true, nil
	})
	return err
}
