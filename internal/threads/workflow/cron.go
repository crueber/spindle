package workflow

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a parsed five-field cron expression in UTC
// (`minute hour day-of-month month day-of-week`), the documented `schedule:`
// subset (15_threads.md §1). Syntax validation lives here — in the parser —
// because a cron the scheduler cannot evaluate must be a PER-FILE parse error
// (the file's other triggers must not silently stop working), never a run that
// mysteriously never fires.
//
// Supported: `*`, a number, a range `a-b`, a step `*/N` or `a-b/N`, and a
// comma list of those (`0,30`, `1-5`, `*/15`). NOT supported (a parse error,
// deliberately): month/weekday names (`MON`), `@daily` macros, `L`/`W`/`#`
// modifiers, and a six-field expression with seconds. Field ranges are
// minute 0-59, hour 0-23, day-of-month 1-31, month 1-12, day-of-week 0-6
// (Sunday = 0; 7 is accepted as Sunday). Day-of-month and day-of-week follow
// the classic Vixie rule: when BOTH are restricted a day matches if EITHER
// matches; when one is `*` only the other applies.
type Cron struct {
	expr string
	sets [5]uint64
	star [5]bool // the field was a bare `*` (no restriction)
}

// CronFieldNames names the five fields for error messages.
var CronFieldNames = [5]string{"minute", "hour", "day-of-month", "month", "day-of-week"}

// cronBounds is the inclusive [lo,hi] range of each field.
var cronBounds = [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}

// ParseCron parses one five-field UTC cron expression.
func ParseCron(expr string) (*Cron, error) {
	trimmed := strings.TrimSpace(expr)
	if trimmed == "" {
		return nil, fmt.Errorf("schedule cron is empty")
	}
	fields := strings.Fields(trimmed)
	if len(fields) != 5 {
		return nil, fmt.Errorf("schedule cron %q must have five fields (minute hour day-of-month month day-of-week), got %d", trimmed, len(fields))
	}
	c := &Cron{expr: trimmed}
	for i, field := range fields {
		lo, hi := cronBounds[i][0], cronBounds[i][1]
		set, isStar, err := parseCronField(field, lo, hi)
		if err != nil {
			return nil, fmt.Errorf("schedule cron %q: %s: %v", trimmed, CronFieldNames[i], err)
		}
		c.sets[i], c.star[i] = set, isStar
	}
	return c, nil
}

// String returns the expression as written (trimmed).
func (c *Cron) String() string {
	if c == nil {
		return ""
	}
	return c.expr
}

// parseCronField parses one field into a bitmask of allowed values.
func parseCronField(field string, lo, hi int) (uint64, bool, error) {
	if field == "*" {
		return fullMask(lo, hi), true, nil
	}
	var mask uint64
	isStar := false
	for _, part := range strings.Split(field, ",") {
		if part == "" {
			return 0, false, fmt.Errorf("empty list element")
		}
		base, stepText, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepText)
			if err != nil || n < 1 {
				return 0, false, fmt.Errorf("bad step %q (must be a positive integer)", stepText)
			}
			step = n
		}
		start, end := lo, hi
		switch {
		case base == "*":
			isStar = true
		case strings.Contains(base, "-"):
			a, b, ok := strings.Cut(base, "-")
			if !ok {
				return 0, false, fmt.Errorf("bad range %q", base)
			}
			from, err := cronNumber(a, lo, hi)
			if err != nil {
				return 0, false, err
			}
			to, err := cronNumber(b, lo, hi)
			if err != nil {
				return 0, false, err
			}
			if from > to {
				return 0, false, fmt.Errorf("range %q starts after it ends", base)
			}
			start, end = from, to
		default:
			n, err := cronNumber(base, lo, hi)
			if err != nil {
				return 0, false, err
			}
			start, end = n, n
		}
		if hasStep && base != "*" && !strings.Contains(base, "-") {
			// `5/15` means "from 5 to the field's maximum, every 15": the
			// classic cron reading, and the only one that makes `*/15` and
			// `5/15` consistent.
			end = hi
		}
		for v := start; v <= end; v += step {
			mask |= 1 << uint(v)
		}
	}
	if mask == 0 {
		return 0, false, fmt.Errorf("matches no value")
	}
	return mask, isStar, nil
}

// cronNumber parses one field value, accepting 7 as Sunday for day-of-week.
func cronNumber(text string, lo, hi int) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		return 0, fmt.Errorf("bad value %q (names and macros are not supported; use numbers)", text)
	}
	if n == 7 && lo == 0 && hi == 6 {
		n = 0 // 7 is Sunday's alias
	}
	if n < lo || n > hi {
		return 0, fmt.Errorf("value %d out of range %d-%d", n, lo, hi)
	}
	return n, nil
}

// fullMask sets every bit in [lo,hi].
func fullMask(lo, hi int) uint64 {
	var mask uint64
	for v := lo; v <= hi; v++ {
		mask |= 1 << uint(v)
	}
	return mask
}

// has reports whether one field allows v.
func has(mask uint64, v int) bool { return mask&(1<<uint(v)) != 0 }

// dayMatches applies the Vixie dom/dow rule: when both are restricted the day
// matches if EITHER field matches; a `*` field defers to the other.
func (c *Cron) dayMatches(t time.Time) bool {
	domOK := has(c.sets[2], t.Day())
	dowOK := has(c.sets[4], int(t.Weekday()))
	switch {
	case c.star[2] && c.star[4]:
		return true
	case c.star[2]:
		return dowOK
	case c.star[4]:
		return domOK
	default:
		return domOK || dowOK
	}
}

// maxCronSearchDays bounds a Next/Prev search (a five-year window covers every
// legal expression, including Feb 29 on a specific weekday).
const maxCronSearchDays = 366 * 5

// Next returns the first tick strictly after t, in t's location (callers pass
// UTC). The bool is false when nothing matches inside the search window (an
// impossible expression such as `0 0 31 2 *`).
func (c *Cron) Next(t time.Time) (time.Time, bool) {
	if c == nil {
		return time.Time{}, false
	}
	cur := t.Truncate(time.Minute).Add(time.Minute)
	day := time.Date(cur.Year(), cur.Month(), cur.Day(), 0, 0, 0, 0, cur.Location())
	for i := 0; i < maxCronSearchDays; i++ {
		if d := day.AddDate(0, 0, i); c.dayMatchesAt(d) {
			if hit, ok := c.firstAfter(d, cur); ok {
				return hit, true
			}
		}
	}
	return time.Time{}, false
}

// Prev returns the most recent tick at or before t, in t's location.
func (c *Cron) Prev(t time.Time) (time.Time, bool) {
	if c == nil {
		return time.Time{}, false
	}
	cur := t.Truncate(time.Minute)
	day := time.Date(cur.Year(), cur.Month(), cur.Day(), 0, 0, 0, 0, cur.Location())
	for i := 0; i < maxCronSearchDays; i++ {
		if d := day.AddDate(0, 0, -i); c.dayMatchesAt(d) {
			if hit, ok := c.lastBefore(d, cur); ok {
				return hit, true
			}
		}
	}
	return time.Time{}, false
}

// dayMatchesAt reports whether the calendar day of d satisfies the month and
// day-of-month/day-of-week fields.
func (c *Cron) dayMatchesAt(d time.Time) bool {
	return has(c.sets[3], int(d.Month())) && c.dayMatches(d)
}

// firstAfter returns the earliest tick on day d at or after notBefore.
func (c *Cron) firstAfter(d, notBefore time.Time) (time.Time, bool) {
	for h := 0; h < 24; h++ {
		if !has(c.sets[1], h) {
			continue
		}
		for m := 0; m < 60; m++ {
			if !has(c.sets[0], m) {
				continue
			}
			candidate := time.Date(d.Year(), d.Month(), d.Day(), h, m, 0, 0, d.Location())
			if !candidate.Before(notBefore) {
				return candidate, true
			}
		}
	}
	return time.Time{}, false
}

// lastBefore returns the latest tick on day d at or before notAfter.
func (c *Cron) lastBefore(d, notAfter time.Time) (time.Time, bool) {
	for h := 23; h >= 0; h-- {
		if !has(c.sets[1], h) {
			continue
		}
		for m := 59; m >= 0; m-- {
			if !has(c.sets[0], m) {
				continue
			}
			candidate := time.Date(d.Year(), d.Month(), d.Day(), h, m, 0, 0, d.Location())
			if !candidate.After(notAfter) {
				return candidate, true
			}
		}
	}
	return time.Time{}, false
}

// validateCronEntries rejects an unsupported `schedule:` entry at parse time
// (the per-file error 15 §1/§2.1 promises) and an empty entry list.
func validateCronEntries(entries []CronEntry) error {
	if len(entries) == 0 {
		return fmt.Errorf("schedule: at least one `- cron: \"…\"` entry is required")
	}
	for i, e := range entries {
		if _, err := ParseCron(e.Cron); err != nil {
			return fmt.Errorf("schedule entry %d: %v", i+1, err)
		}
	}
	return nil
}
