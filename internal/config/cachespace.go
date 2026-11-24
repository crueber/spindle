// cachespace.go — cache-space safety (BUG-6). The compiled cache budget
// (cache.max_bytes, 20 GiB) assumes the cache directory lives on a volume that
// large; on a small/tmpfs data dir it never fits, so a push dies mid-ingest
// with ENOSPC and the client only sees "unpacker error". At boot, compare the
// budget with the filesystem holding cache.dir: a compiled-in default that
// cannot fit is lowered (with a log line), a value the file or env set is
// reported but never changed.
package config

import "fmt"

// cacheFSSize is the capacity + type of the filesystem holding a directory.
type cacheFSSize struct {
	FSType string
	Total  int64
	// Avail is the space available to an unprivileged writer (Bavail);
	// the boot guard only needs Total, the Administration page shows both.
	Avail int64
}

// CacheSpaceInfo is the measured filesystem behind a directory: the same
// capacity facts the boot-time cache guard and the disk eviction watermark
// use, exposed read-only for the Administration → Maintenance page.
type CacheSpaceInfo struct {
	FSType string
	Total  int64 // bytes on the filesystem
	Avail  int64 // bytes free to an unprivileged writer
}

// CacheSpace stats the filesystem holding dir. It is a single statfs (no
// directory walk) and never a correctness input: ok=false off Linux, when dir
// is empty, or when it does not exist on this host.
func CacheSpace(dir string) (CacheSpaceInfo, bool) {
	fs, err := cacheFilesystem(dir)
	if err != nil || fs.Total <= 0 {
		return CacheSpaceInfo{}, false
	}
	return CacheSpaceInfo{FSType: fs.FSType, Total: fs.Total, Avail: fs.Avail}, true
}

// statCacheDir is a test hook (see cachespace_linux.go / cachespace_other.go).
var statCacheDir = cacheFilesystem

// AdaptCacheBudget inspects the filesystem holding cache.dir and returns an
// operator-facing notice, or "" when the configured budget fits. A notice
// describing a lowered default is informational; a notice about an explicit
// cache.max_bytes is a warning (the value is left alone).
func AdaptCacheBudget(c *Config) string {
	if c == nil || c.Cache.Dir == "" {
		return ""
	}
	max := int64(c.Cache.MaxBytes)
	if max <= 0 {
		return ""
	}
	info, err := statCacheDir(c.Cache.Dir)
	if err != nil || info.Total <= 0 || max <= info.Total {
		return ""
	}
	// Headroom consistent with the disk-mode eviction watermark: the budget
	// is the eviction trigger, so it must sit below the filesystem's size.
	frac := c.Cache.DiskHighWatermark
	if frac <= 0 || frac >= 1 {
		frac = 0.9
	}
	limit := int64(float64(info.Total) * frac)
	limit = limit / (1 << 20) * (1 << 20) // whole MiB, so ByteSize renders cleanly
	if limit <= 0 {
		return ""
	}
	fsDesc := "the filesystem"
	if info.FSType != "" {
		fsDesc = "the " + info.FSType + " filesystem"
	}
	if c.Explicit("cache.max_bytes") {
		return fmt.Sprintf(
			"cache.max_bytes=%s exceeds %s holding cache.dir %s (%s total): the cache cannot fit its budget and writes will fail with ENOSPC before eviction helps — lower cache.max_bytes or move cache.dir to a larger filesystem",
			ByteSize(max), fsDesc, c.Cache.Dir, ByteSize(info.Total))
	}
	old := c.Cache.MaxBytes
	c.Cache.MaxBytes = ByteSize(limit)
	return fmt.Sprintf(
		"cache.dir %s is on %s with %s total, smaller than the default cache.max_bytes (%s): lowered cache.max_bytes to %s (the eviction watermark) so writes stay within the filesystem; set cache.max_bytes explicitly to override",
		c.Cache.Dir, fsDesc, ByteSize(info.Total), old, ByteSize(limit))
}
