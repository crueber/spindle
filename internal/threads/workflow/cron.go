package workflow

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	// The runtime image carries no tz database; a per-entry `timezone:` is
	// part of the documented cron surface, so the IANA data is embedded
	// (stdlib, no new module).
	_ "time/tzdata"
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
	// The day-of-month / day-of-week modifiers the dialect accepts.
	domLast       bool
	domLastOffset int // `L-3`: the third-to-last day (0 for bare L)
	domNearest    int // `15W`: nearest weekday to the 15th (0 = unused)
	dowLast       uint64
	nth           [8]int // weekday → `#n` (0 = none)
}

// cronMacros are the documented `@` shorthands (the standard cron set).
var cronMacros = map[string]string{
	"@yearly":   "0 0 1 1 *",
	"@annually": "0 0 1 1 *",
	"@monthly":  "0 0 1 * *",
	"@weekly":   "0 0 * * 0",
	"@daily":    "0 0 * * *",
	"@midnight": "0 0 * * *",
	"@hourly":   "0 * * * *",
}

var (
	cronMonthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
	cronDayNames   = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}
	cronLastRe     = regexp.MustCompile(`(?i)^L(?:-(\d{1,2}))?$`)
	cronNearestRe  = regexp.MustCompile(`(?i)^(\d{1,2})W$`)
	cronWeekLastRe = regexp.MustCompile(`(?i)^([0-9]|[A-Za-z]{3})L$`)
	cronNthRe      = regexp.MustCompile(`(?i)^([0-9]|[A-Za-z]{3})#([1-5])$`)
)

// CronFieldNames names the five fields for error messages.
var CronFieldNames = [5]string{"minute", "hour", "day-of-month", "month", "day-of-week"}

// cronBounds is the inclusive [lo,hi] range of each field.
var cronBounds = [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}

// ParseCron parses one five-field cron expression. `@daily`-family macros are
// accepted as the standard shorthands they spell.
func ParseCron(expr string) (*Cron, error) {
	trimmed := strings.TrimSpace(expr)
	if trimmed == "" {
		return nil, fmt.Errorf("schedule cron is empty")
	}
	if macro, ok := cronMacros[strings.ToLower(trimmed)]; ok {
		trimmed = macro
	} else if strings.HasPrefix(trimmed, "@") {
		return nil, fmt.Errorf("schedule cron %q: unknown macro (supported: @yearly, @annually, @monthly, @weekly, @daily, @midnight, @hourly)", expr)
	}
	fields := strings.Fields(trimmed)
	if len(fields) != 5 {
		return nil, fmt.Errorf("schedule cron %q must have five fields (minute hour day-of-month month day-of-week), got %d", expr, len(fields))
	}
	c := &Cron{expr: strings.TrimSpace(expr)}
	for i, field := range fields {
		lo, hi := cronBounds[i][0], cronBounds[i][1]
		set, isStar, err := c.parseCronField(i, field, lo, hi)
		if err != nil {
			return nil, fmt.Errorf("schedule cron %q: %s: %v", expr, CronFieldNames[i], err)
		}
		c.sets[i], c.star[i] = set, isStar
	}
	return c, nil
}

// parseCronField parses one field, including the day-of-month `L`/`W` and
// day-of-week `L`/`#` forms. A modifier stands on its own (it cannot be mixed
// into a comma list with a step or range), which is the documented dialect.
func (c *Cron) parseCronField(index int, field string, lo, hi int) (uint64, bool, error) {
	switch index {
	case 2:
		if m := cronLastRe.FindStringSubmatch(field); m != nil {
			offset := 0
			if m[1] != "" {
				n, err := strconv.Atoi(m[1])
				if err != nil || n > 30 {
					return 0, false, fmt.Errorf("last-day offset must be 0-30")
				}
				offset = n
			}
			c.domLast, c.domLastOffset = true, offset
			return 0, false, nil
		}
		if m := cronNearestRe.FindStringSubmatch(field); m != nil {
			n, _ := strconv.Atoi(m[1])
			if n < 1 || n > 31 {
				return 0, false, fmt.Errorf("nearest weekday %d out of range 1-31", n)
			}
			c.domNearest = n
			return 0, false, nil
		}
	case 4:
		if m := cronWeekLastRe.FindStringSubmatch(field); m != nil {
			day, err := cronDayValue(m[1])
			if err != nil {
				return 0, false, err
			}
			c.dowLast |= 1 << uint(day)
			return 0, false, nil
		}
		if m := cronNthRe.FindStringSubmatch(field); m != nil {
			day, err := cronDayValue(m[1])
			if err != nil {
				return 0, false, err
			}
			n, _ := strconv.Atoi(m[2])
			c.nth[day] = n
			return 0, false, nil
		}
	}
	if strings.ContainsAny(field, "LW#") {
		return 0, false, fmt.Errorf("%q is not a supported special form", field)
	}
	return parseCronField(field, index, lo, hi)
}

// cronDayValue resolves a day-of-week token (number or SUN-SAT name).
func cronDayValue(text string) (int, error) {
	if n, err := strconv.Atoi(text); err == nil {
		if n == 7 {
			n = 0
		}
		if n < 0 || n > 6 {
			return 0, fmt.Errorf("day-of-week %d out of range 0-6", n)
		}
		return n, nil
	}
	if d, ok := cronDayNames[strings.ToLower(text)]; ok {
		return d, nil
	}
	return 0, fmt.Errorf("unknown day-of-week %q", text)
}

// String returns the expression as written (trimmed).
func (c *Cron) String() string {
	if c == nil {
		return ""
	}
	return c.expr
}

// parseCronField parses one field into a bitmask of allowed values.
func parseCronField(field string, index, lo, hi int) (uint64, bool, error) {
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
			from, err := cronNumber(index, a, lo, hi)
			if err != nil {
				return 0, false, err
			}
			to, err := cronNumber(index, b, lo, hi)
			if err != nil {
				return 0, false, err
			}
			if from > to {
				return 0, false, fmt.Errorf("range %q starts after it ends", base)
			}
			start, end = from, to
		default:
			n, err := cronNumber(index, base, lo, hi)
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

// cronNumber parses one field value, accepting a three-letter month/weekday
// name in the month and day-of-week fields and 7 as Sunday's alias.
func cronNumber(index int, text string, lo, hi int) (int, error) {
	trimmed := strings.TrimSpace(text)
	n, err := strconv.Atoi(trimmed)
	if err != nil {
		var ok bool
		switch index {
		case 3:
			n, ok = cronMonthNames[strings.ToLower(trimmed)]
		case 4:
			n, ok = cronDayNames[strings.ToLower(trimmed)]
		}
		if !ok {
			return 0, fmt.Errorf("bad value %q (use a number%s)", text, cronNameHint(index))
		}
	}
	if n == 7 && lo == 0 && hi == 6 {
		n = 0 // 7 is Sunday's alias
	}
	if n < lo || n > hi {
		return 0, fmt.Errorf("value %d out of range %d-%d", n, lo, hi)
	}
	return n, nil
}

// cronNameHint names the accepted spellings for a field error.
func cronNameHint(index int) string {
	switch index {
	case 3:
		return " or a month name (JAN-DEC)"
	case 4:
		return " or a weekday name (SUN-SAT)"
	}
	return ""
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
// matches if EITHER field matches; a `*` field defers to the other. The
// `L`/`W`/`#` modifiers are restrictions in their own field.
func (c *Cron) dayMatches(t time.Time) bool {
	domOK := has(c.sets[2], t.Day()) || c.domSpecialMatches(t)
	dowOK := has(c.sets[4], int(t.Weekday())) || c.dowSpecialMatches(t)
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

// domSpecialMatches applies `L`, `L-N` and `NW`.
func (c *Cron) domSpecialMatches(t time.Time) bool {
	last := daysInMonth(t.Year(), t.Month())
	if c.domLast {
		want := last - c.domLastOffset
		if want < 1 {
			want = 1
		}
		return t.Day() == want
	}
	if c.domNearest > 0 {
		return t.Day() == nearestWeekday(t.Year(), t.Month(), c.domNearest)
	}
	return false
}

// dowSpecialMatches applies `DL` (last weekday of the month) and `D#n`.
func (c *Cron) dowSpecialMatches(t time.Time) bool {
	wd := int(t.Weekday())
	if c.dowLast&(1<<uint(wd)) != 0 && t.Day()+7 > daysInMonth(t.Year(), t.Month()) {
		return true
	}
	if n := c.nth[wd]; n > 0 {
		return (t.Day()-1)/7+1 == n
	}
	return false
}

// daysInMonth is the calendar length of one month.
func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// nearestWeekday is cron's `NW`: the weekday closest to day N, never leaving
// the month (Saturday moves back to Friday, Sunday forward to Monday, with the
// month boundary clamping).
func nearestWeekday(year int, month time.Month, day int) int {
	last := daysInMonth(year, month)
	if day > last {
		day = last
	}
	switch time.Date(year, month, day, 0, 0, 0, 0, time.UTC).Weekday() {
	case time.Saturday:
		if day == 1 {
			return 3
		}
		return day - 1
	case time.Sunday:
		if day == last {
			return day - 2
		}
		return day + 1
	}
	return day
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
			if !sameWallClock(candidate, d.Year(), d.Month(), d.Day(), h, m) {
				continue // a local time skipped by a DST spring-forward
			}
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
			if !sameWallClock(candidate, d.Year(), d.Month(), d.Day(), h, m) {
				continue // a local time skipped by a DST spring-forward
			}
			if !candidate.After(notAfter) {
				return candidate, true
			}
		}
	}
	return time.Time{}, false
}

// sameWallClock reports whether a constructed time really names the requested
// local wall clock. time.Date normalises a nonexistent local time (the DST
// spring-forward gap) into the following hour; comparing the components is how
// the scheduler refuses to fire a tick that does not exist.
func sameWallClock(t time.Time, year int, month time.Month, day, hour, minute int) bool {
	y, mo, d := t.Date()
	h, m, _ := t.Clock()
	return y == year && mo == month && d == day && h == hour && m == minute
}

// Location resolves a schedule entry's `timezone:` ("" = UTC).
func (e CronEntry) Location() (*time.Location, error) {
	tz := strings.TrimSpace(e.Timezone)
	if tz == "" || strings.EqualFold(tz, "UTC") {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("unknown timezone %q (use an IANA name such as Europe/Berlin)", e.Timezone)
	}
	return loc, nil
}

// PrevIn is Prev evaluated on the entry's local clock. A missed local time (a
// DST spring-forward gap) has no tick; a repeated local time (a fall-back hour)
// yields the same wall-clock slot either way, which is what the scheduler's
// slot watermark coalesces to one fire.
func (c *Cron) PrevIn(now time.Time, loc *time.Location) (time.Time, bool) {
	if loc == nil {
		loc = time.UTC
	}
	return c.Prev(now.In(loc))
}

// NextIn is Next evaluated on the entry's local clock.
func (c *Cron) NextIn(now time.Time, loc *time.Location) (time.Time, bool) {
	if loc == nil {
		loc = time.UTC
	}
	return c.Next(now.In(loc))
}

// Slot is the coalescing key of one tick: the LOCAL wall-clock minute the
// schedule targeted. Two instants that name the same local minute (the DST
// fall-back hour) share a slot, so the scheduler fires once.
func (c *Cron) Slot(t time.Time) string { return t.Format("2006-01-02T15:04") }

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
		if _, err := e.Location(); err != nil {
			return fmt.Errorf("schedule entry %d: %v", i+1, err)
		}
	}
	return nil
}
