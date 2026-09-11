package cron

import (
	"strings"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04", s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func TestExpressionsMatchTheMinutesTheyName(t *testing.T) {
	for _, tc := range []struct {
		expr string
		yes  []string
		no   []string
	}{
		// The examples from Vercel's own table.
		{"5 * * * *", []string{"2026-09-10 12:05", "2026-09-10 00:05"}, []string{"2026-09-10 12:04", "2026-09-10 12:06"}},
		{"* 5 * * *", []string{"2026-09-10 05:00", "2026-09-10 05:59"}, []string{"2026-09-10 06:00"}},
		{"* * 5 * *", []string{"2026-09-05 13:37"}, []string{"2026-09-06 13:37"}},
		{"* * * 5 *", []string{"2026-05-20 08:00"}, []string{"2026-06-20 08:00"}},
		// 2026-09-11 is a Friday.
		{"* * * * 5", []string{"2026-09-11 08:00"}, []string{"2026-09-10 08:00"}},
		// Steps, lists, ranges.
		{"*/15 * * * *", []string{"2026-09-10 12:00", "2026-09-10 12:15", "2026-09-10 12:45"}, []string{"2026-09-10 12:10"}},
		{"0 9-17 * * 1-5", []string{"2026-09-10 09:00", "2026-09-10 17:00"}, []string{"2026-09-10 18:00", "2026-09-12 12:00", "2026-09-10 09:01"}},
		{"0 5 1,15 * *", []string{"2026-09-01 05:00", "2026-09-15 05:00"}, []string{"2026-09-02 05:00"}},
		{"30 2 * * *", []string{"2026-09-10 02:30"}, []string{"2026-09-10 02:31", "2026-09-10 03:30"}},
		{"5/10 * * * *", []string{"2026-09-10 12:05", "2026-09-10 12:55"}, []string{"2026-09-10 12:00"}},
		// The shorthands.
		{"@hourly", []string{"2026-09-10 12:00"}, []string{"2026-09-10 12:01"}},
		{"@daily", []string{"2026-09-10 00:00"}, []string{"2026-09-10 01:00"}},
		{"@weekly", []string{"2026-09-13 00:00"}, []string{"2026-09-14 00:00"}}, // the 13th is a Sunday
		{"@monthly", []string{"2026-10-01 00:00"}, []string{"2026-10-02 00:00"}},
	} {
		t.Run(tc.expr, func(t *testing.T) {
			spec, err := Parse(tc.expr)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			for _, s := range tc.yes {
				if !spec.Matches(at(s)) {
					t.Errorf("%q should match %s", tc.expr, s)
				}
			}
			for _, s := range tc.no {
				if spec.Matches(at(s)) {
					t.Errorf("%q should not match %s", tc.expr, s)
				}
			}
		})
	}
}

// Seconds never matter, and the location of the time never matters: the
// expression is read against UTC.
func TestMatchesIgnoresSecondsAndConvertsToUTC(t *testing.T) {
	spec, _ := Parse("30 12 * * *")
	utc := time.Date(2026, 9, 10, 12, 30, 59, 0, time.UTC)
	if !spec.Matches(utc) {
		t.Error("seconds should not stop a match")
	}
	east := time.FixedZone("east", 3*3600)
	if !spec.Matches(utc.In(east)) {
		t.Error("a time in another zone should be read as UTC")
	}
	if spec.Matches(time.Date(2026, 9, 10, 12, 30, 0, 0, east)) {
		t.Error("12:30 in UTC+3 is 09:30 UTC and should not match")
	}
}

func TestRefusalsNameTheProblem(t *testing.T) {
	for _, tc := range []struct{ expr, want string }{
		{"* * * *", "5 fields"},
		{"* * * * * *", "5 fields"},
		{"60 * * * *", "minute: 60"},
		{"* 24 * * *", "hour: 24"},
		{"* * 0 * *", "day of month: 0"},
		{"* * * 13 *", "month: 13"},
		{"* * * * 7", "day of week: 7"},
		{"0 0 * * MON", "MON"},
		{"* * 5 * 5", "cannot both be set"},
		{"*/0 * * * *", "step"},
		{"5-3 * * * *", "backwards"},
		{"", "5 fields"},
		{"@yearly", "5 fields"},
	} {
		_, err := Parse(tc.expr)
		if err == nil {
			t.Errorf("%q was accepted", tc.expr)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: error %q does not mention %q", tc.expr, err, tc.want)
		}
	}
}

func TestTheZeroSpecMatchesNothing(t *testing.T) {
	if (Spec{}).Matches(time.Now()) {
		t.Error("a zero Spec must never fire")
	}
}

func TestStringExpandsTheShorthand(t *testing.T) {
	spec, _ := Parse("@daily")
	if spec.String() != "0 0 * * *" {
		t.Errorf("String() = %q", spec.String())
	}
}
