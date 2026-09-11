package cli

import (
	"strings"
	"testing"
)

// --schedule is one flag for two kinds of job, and the kind is spelled out:
// "GET /path" is a request, anything else is a command. Inferring a request
// from a leading slash turned /usr/local/bin/backup.sh into a nightly 404.
func TestParseScheduleSpellsTheKindOut(t *testing.T) {
	for _, tc := range []struct {
		in         string
		cron, path string
		cmd        string
	}{
		{"0 5 * * * GET /jobs/digest", "0 5 * * *", "/jobs/digest", ""},
		{"@hourly get /jobs/tick", "@hourly", "/jobs/tick", ""},
		{"@hourly /usr/local/bin/backup.sh", "@hourly", "", "/usr/local/bin/backup.sh"},
		{"*/5 * * * * date >> /tmp/ticks", "*/5 * * * *", "", "date >> /tmp/ticks"},
		{"0 0 * * 0 ./bin/rotate --keep 7", "0 0 * * 0", "", "./bin/rotate --keep 7"},
	} {
		got, err := parseSchedule(tc.in)
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if got.Cron != tc.cron || got.Path != tc.path || got.Cmd != tc.cmd {
			t.Errorf("%q = %+v, want cron %q path %q cmd %q", tc.in, got, tc.cron, tc.path, tc.cmd)
		}
	}

	for _, bad := range []string{
		"",
		"0 5 * * *",           // no target
		"@hourly",             // no target
		"0 5 * * * GET",       // GET with no path
		"0 5 * * * GET jobs",  // not a path
		"0 5 * * * GET /a /b", // two paths
	} {
		if _, err := parseSchedule(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		} else if !strings.Contains(err.Error(), "schedule") {
			t.Errorf("%q: error %q does not mention the flag", bad, err)
		}
	}
}
