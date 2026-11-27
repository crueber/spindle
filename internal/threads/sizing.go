package threads

// Larger-runner sizing (PAR-176).
//
// Spindle has no hosted size classes. A runner may DECLARE the ceiling it
// offers (`--max-cpus 4 --max-memory 16GB`, recorded on its registry record);
// a job whose `container.options` asks for more than a declaring runner offers
// is simply not offered to it, and the queue reason names the request. A job's
// own `container.options` already reaches the daemon, and Docker enforces it —
// this is a scheduling filter, never a second enforcement point.
//
// Zero on either field means "undeclared": the daemon's own limits apply, and
// every job matches. That keeps a fleet on a single big host unchanged.

import (
	"fmt"
	"strconv"
	"strings"
)

// JobSize is the resource request parsed from a job's container options.
type JobSize struct {
	CPUs        float64
	MemoryBytes int64
}

// IsZero reports whether the job asked for nothing (the common case).
func (s JobSize) IsZero() bool { return s.CPUs == 0 && s.MemoryBytes == 0 }

// Describe renders the request the way the queue reason and docs spell it.
func (s JobSize) Describe() string {
	parts := make([]string, 0, 2)
	if s.CPUs > 0 {
		parts = append(parts, trimFloat(s.CPUs)+" cpu")
	}
	if s.MemoryBytes > 0 {
		parts = append(parts, formatMemory(s.MemoryBytes))
	}
	return strings.Join(parts, " / ")
}

// ParseJobSize reads `--cpus` and `--memory`/`-m` from a container option
// string with the same splitter the runner uses. A malformed value is ignored
// here: the container-options validator owns refusal, and a value it accepted
// parses here (both use the same numeric grammar).
func ParseJobSize(options string) JobSize {
	var size JobSize
	toks := SplitOptions(options)
	for i := 0; i < len(toks); i++ {
		name, value, hasValue := containerOptionToken(toks[i])
		if !hasValue {
			if i+1 < len(toks) {
				value = toks[i+1]
				i++
			} else {
				continue
			}
		}
		switch name {
		case "--cpus":
			if v, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil && v > 0 {
				size.CPUs = v
			}
		case "--memory", "-m", "--memory-swap":
			// --memory is the job's ceiling; --memory-swap only extends it, so
			// it is not a larger request than --memory for scheduling.
			if name == "--memory-swap" {
				continue
			}
			if v, ok := parseMemoryBytes(value); ok {
				size.MemoryBytes = v
			}
		}
	}
	return size
}

// JobSizeOf is a job's requested size (container.options only; a job without a
// spec asks for nothing).
func JobSizeOf(job *Job) JobSize {
	if job == nil || job.Spec == nil || job.Spec.Container == nil {
		return JobSize{}
	}
	return ParseJobSize(job.Spec.Container.Options)
}

// RunnerOffers reports whether a runner's declared ceiling admits this size. An
// undeclared (zero) ceiling offers everything; a declared one must be at least
// the request on both axes.
func RunnerOffers(doc *RunnerDoc, size JobSize) bool {
	if doc == nil || size.IsZero() {
		return true
	}
	if doc.MaxCPUs > 0 && size.CPUs > doc.MaxCPUs {
		return false
	}
	if doc.MaxMemoryBytes > 0 && size.MemoryBytes > doc.MaxMemoryBytes {
		return false
	}
	return true
}

// clampRunnerSizing bounds a declared ceiling to sane values. Negative values
// are an operator mistake and clamp to "undeclared"; the CLI validates first.
func clampRunnerSizing(cpus float64, memoryBytes int64) (float64, int64) {
	if cpus < 0 || cpus > 1024 {
		cpus = 0
	}
	if memoryBytes < 0 || memoryBytes > 1<<50 {
		memoryBytes = 0
	}
	return cpus, memoryBytes
}

// parseMemoryBytes parses docker's memory grammar: a number with an optional
// b/k/m/g (decimal) or kb/kib/mb/mib/gb/gib (binary) suffix.
func parseMemoryBytes(text string) (int64, bool) {
	s := strings.ToLower(strings.TrimSpace(text))
	if s == "" {
		return 0, false
	}
	mult := int64(1)
	for _, suffix := range []struct {
		s string
		m int64
	}{
		{"gib", 1 << 30}, {"gb", 1 << 30}, {"g", 1 << 30},
		{"mib", 1 << 20}, {"mb", 1 << 20}, {"m", 1 << 20},
		{"kib", 1 << 10}, {"kb", 1 << 10}, {"k", 1 << 10},
		{"b", 1},
	} {
		if strings.HasSuffix(s, suffix.s) {
			mult = suffix.m
			s = strings.TrimSuffix(s, suffix.s)
			break
		}
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v < 0 {
		return 0, false
	}
	return int64(v * float64(mult)), true
}

// formatMemory renders bytes with a binary unit, dropping a trailing .0.
func formatMemory(bytes int64) string {
	switch {
	case bytes >= 1<<30:
		return trimFloat(float64(bytes)/(1<<30)) + " GiB"
	case bytes >= 1<<20:
		return trimFloat(float64(bytes)/(1<<20)) + " MiB"
	case bytes >= 1<<10:
		return trimFloat(float64(bytes)/(1<<10)) + " KiB"
	default:
		return strconv.FormatInt(bytes, 10) + " B"
	}
}

// ParseMemorySize is parseMemoryBytes for the CLI (a declared ceiling).
func ParseMemorySize(text string) (int64, bool) { return parseMemoryBytes(text) }

// trimFloat renders a float without a trailing ".0".
func trimFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// ValidateRunnerSizing validates the CLI's declared ceiling before startup.
func ValidateRunnerSizing(cpus float64, memoryBytes int64) error {
	if cpus < 0 || cpus > 1024 {
		return fmt.Errorf("max cpus must be 0 (undeclared) or 0.1…1024, got %v", cpus)
	}
	if memoryBytes < 0 || memoryBytes > 1<<50 {
		return fmt.Errorf("max memory must be 0 (undeclared) or a positive size, got %d", memoryBytes)
	}
	return nil
}
