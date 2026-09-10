// Package cron matches wall-clock minutes against five-field cron
// expressions.
//
// It is the subset every platform that schedules over HTTP has settled on
// (Vercel's, near enough): five numeric fields -- minute, hour, day of month,
// month, day of week -- with `*`, lists, ranges and steps, in UTC. No names
// (`MON`, `JAN`), no seconds, no years, and day-of-month and day-of-week may
// not both be restricted: classic cron ORs those two when both are set, which
// nobody remembers, so refusing the combination is kinder than honouring it.
// The four `@` shorthands people actually type are accepted.
//
// Written here rather than pulled in: the whole thing is a hundred lines, and
// a dependency for it would be a dependency for convenience.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Spec is a parsed expression. The zero value matches nothing.
type Spec struct {
	min, hour, dom, mon, dow uint64 // bitsets: bit v set means value v matches
	expr                     string
}

// The four shorthands, spelled out.
var aliases = map[string]string{
	"@hourly":  "0 * * * *",
	"@daily":   "0 0 * * *",
	"@weekly":  "0 0 * * 0",
	"@monthly": "0 0 1 * *",
}

type field struct {
	name   string
	lo, hi int
}

var fields = [5]field{
	{"minute", 0, 59}, {"hour", 0, 23}, {"day of month", 1, 31}, {"month", 1, 12}, {"day of week", 0, 6},
}

// Parse reads an expression. The error names the field and the value, so it
// can be shown to the person who typed it.
func Parse(expr string) (Spec, error) {
	expr = strings.TrimSpace(expr)
	if alias, ok := aliases[expr]; ok {
		expr = alias
	}
	parts := strings.Fields(expr)
	if len(parts) != 5 {
		return Spec{}, fmt.Errorf("cron %q: want 5 fields (minute hour day-of-month month day-of-week), got %d", expr, len(parts))
	}
	var bits [5]uint64
	var star [5]bool
	for i, part := range parts {
		b, isStar, err := parseField(part, fields[i])
		if err != nil {
			return Spec{}, fmt.Errorf("cron %q: %w", expr, err)
		}
		bits[i], star[i] = b, isStar
	}
	if !star[2] && !star[4] {
		return Spec{}, fmt.Errorf("cron %q: day of month and day of week cannot both be set; leave one as *", expr)
	}
	return Spec{min: bits[0], hour: bits[1], dom: bits[2], mon: bits[3], dow: bits[4], expr: expr}, nil
}

// parseField reads one field: a comma list of `*`, `*/n`, `a`, `a-b` or
// `a-b/n`. star reports whether the field was `*` alone, which is what the
// day-of-month/day-of-week rule needs to know.
func parseField(s string, f field) (bits uint64, star bool, err error) {
	if s == "*" {
		return rangeBits(f.lo, f.hi, 1), true, nil
	}
	for _, item := range strings.Split(s, ",") {
		step := 1
		if at := strings.IndexByte(item, '/'); at >= 0 {
			step, err = strconv.Atoi(item[at+1:])
			if err != nil || step < 1 {
				return 0, false, fmt.Errorf("%s: step %q must be a positive number", f.name, item[at+1:])
			}
			item = item[:at]
		}
		lo, hi := f.lo, f.hi
		switch {
		case item == "*":
			// */n: every n across the whole range.
		case strings.Contains(item, "-"):
			a, b, ok := strings.Cut(item, "-")
			if lo, err = atoi(a, f); err != nil {
				return 0, false, err
			}
			if hi, err = atoi(b, f); err != nil || !ok {
				return 0, false, err
			}
			if lo > hi {
				return 0, false, fmt.Errorf("%s: range %q runs backwards", f.name, item)
			}
		default:
			if lo, err = atoi(item, f); err != nil {
				return 0, false, err
			}
			hi = lo
			if step != 1 {
				// "5/10" means from 5 to the end, every 10 -- the one place a
				// single value implies a range.
				hi = f.hi
			}
		}
		bits |= rangeBits(lo, hi, step)
	}
	return bits, false, nil
}

func atoi(s string, f field) (int, error) {
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a number (names such as MON or JAN are not supported)", f.name, s)
	}
	if v < f.lo || v > f.hi {
		return 0, fmt.Errorf("%s: %d is outside %d..%d", f.name, v, f.lo, f.hi)
	}
	return v, nil
}

func rangeBits(lo, hi, step int) uint64 {
	var bits uint64
	for v := lo; v <= hi; v += step {
		bits |= 1 << uint(v)
	}
	return bits
}

func has(bits uint64, v int) bool { return bits&(1<<uint(v)) != 0 }

// Matches reports whether the minute containing t is one the expression
// names. Seconds are ignored; the clock is UTC whatever t's location.
func (s Spec) Matches(t time.Time) bool {
	t = t.UTC()
	return has(s.min, t.Minute()) && has(s.hour, t.Hour()) &&
		has(s.dom, t.Day()) && has(s.mon, int(t.Month())) && has(s.dow, int(t.Weekday()))
}

// String is the expression as parsed, with a shorthand expanded.
func (s Spec) String() string { return s.expr }
