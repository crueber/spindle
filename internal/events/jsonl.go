// jsonl.go — the `jsonl` events sink (14_extensibility.md §14.6): one JSON
// object per line, rotated by size, on a local path.
//
// Why a local path and not a bucket object family (the §14.6 sketch's
// wording): the bucket is the repository and its object PUT replaces the whole
// object, so an append-only trail there is a read-modify-write per event (an
// extra GET+PUT on every catch-up, racy across instances, and a lost race
// drops frames). An audit export is derived output — like a log file — not
// repository state; the doc records the deviation and its rationale in the
// same change. The frames are the SAME objects the webhook sink sends as a
// JSON array, so the two sinks cannot drift.
package events

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// JSONL sink defaults (config.EventSinkJSONL documents the same values).
const (
	DefaultJSONLMaxBytes int64 = 64 << 20
	DefaultJSONLKeep           = 3
)

// JSONLSink appends one NDJSON frame per event to Path. Rotation: when the
// next batch would push the file past MaxBytes, Path is renamed to Path.1
// (shifting Path.1…Path.Keep-1 up one, dropping the oldest) and a fresh file
// starts. A single batch larger than MaxBytes is still written whole — losing
// events is worse than an oversized file.
type JSONLSink struct {
	Path     string
	MaxBytes int64
	Keep     int
	Label    string // metric label; "" → "jsonl"

	mu sync.Mutex // one writer per process; O_APPEND keeps instances line-atomic
}

// Name implements Sink.
func (s *JSONLSink) Name() string {
	if s != nil && s.Label != "" {
		return s.Label
	}
	return KindJSONL
}

// Deliver implements Sink. A write failure is returned so the bridge leaves
// its cursor untouched (fail closed: no event is silently dropped from the
// trail).
func (s *JSONLSink) Deliver(ctx context.Context, repo string, batch []RefEvent) error {
	if len(batch) == 0 {
		return nil
	}
	var buf bytes.Buffer
	for i := range batch {
		frame, err := json.Marshal(&batch[i])
		if err != nil {
			return fmt.Errorf("jsonl: encode event: %w", err)
		}
		buf.Write(frame)
		buf.WriteByte('\n')
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	maxBytes := s.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultJSONLMaxBytes
	}
	keep := s.Keep
	if keep <= 0 {
		keep = DefaultJSONLKeep
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return appendNDJSON(s.Path, buf.Bytes(), maxBytes, keep)
}

// appendNDJSON appends frame (one or more complete NDJSON lines) to path,
// rotating first when needed. The caller holds the sink mutex.
func appendNDJSON(path string, frame []byte, maxBytes int64, keep int) error {
	if path == "" {
		return fmt.Errorf("jsonl: empty path")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("jsonl: create dir %s: %w", dir, err)
		}
	}
	if info, err := os.Stat(path); err == nil {
		if info.Size() > 0 && info.Size()+int64(len(frame)) > maxBytes {
			if err := rotateNDJSON(path, keep); err != nil {
				return err
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("jsonl: stat %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("jsonl: open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(frame); err != nil {
		return fmt.Errorf("jsonl: append %s: %w", path, err)
	}
	return nil
}

// rotateNDJSON shifts path→path.1, path.1→path.2 … keeping at most keep
// rotated files. The rename order runs oldest-first so no destination is
// overwritten before its source has moved.
func rotateNDJSON(path string, keep int) error {
	if keep < 1 {
		keep = 1
	}
	_ = os.Remove(fmt.Sprintf("%s.%d", path, keep))
	for i := keep - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", path, i)
		dst := fmt.Sprintf("%s.%d", path, i+1)
		if _, err := os.Stat(src); err == nil {
			if err := os.Rename(src, dst); err != nil {
				return fmt.Errorf("jsonl: rotate %s: %w", src, err)
			}
		}
	}
	if err := os.Rename(path, path+".1"); err != nil {
		return fmt.Errorf("jsonl: rotate %s: %w", path, err)
	}
	return nil
}
