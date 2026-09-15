package pilots

import (
	"strings"
	"testing"
)

// Every engine's command emits key=value pairs, so ONE parser reads all four.
// Four output formats would be four parsers, and three of them would be wrong
// the first time an engine changed its display.
func TestEveryEngineEmitsKeyValuePairs(t *testing.T) {
	for _, engine := range []string{"postgres", "mysql", "redis", "mongo"} {
		cmd := MetricsCommand(engine)
		if cmd == "" {
			t.Errorf("%s has no metrics command", engine)
			continue
		}
		// Each either produces `=` natively or translates its separator into
		// one. A command that did neither would parse as nothing at all.
		if !strings.Contains(cmd, "=") {
			t.Errorf("%s's command produces no key=value output: %s", engine, cmd)
		}
	}
	if MetricsCommand("cassandra") != "" {
		t.Error("an engine with no recipe was given a metrics command")
	}
}

// Postgres reports the pair that matters -- connections against the limit --
// and enough to compute a cache hit ratio. Either number alone says nothing
// about how close the database is to refusing work.
func TestPostgresMetricsParse(t *testing.T) {
	got := ParseEngineMetrics("postgres", strings.Join([]string{
		"connections=42",
		"max_connections=100",
		"commits=1500",
		"rollbacks=3",
		"blks_hit=9900",
		"blks_read=100",
		"data_bytes=524288000",
		"uptime=86400",
	}, "\n"))

	if got.Connections != 42 || got.MaxConnections != 100 {
		t.Errorf("connections = %d/%d, want 42/100", got.Connections, got.MaxConnections)
	}
	if got.Commits != 1500 || got.Rollbacks != 3 {
		t.Errorf("commits/rollbacks = %d/%d, want 1500/3", got.Commits, got.Rollbacks)
	}
	if got.CacheHitRatio < 0.98 || got.CacheHitRatio > 1.0 {
		t.Errorf("cache hit ratio = %f, want ~0.99", got.CacheHitRatio)
	}
	if got.DataBytes != 524288000 {
		t.Errorf("data_bytes = %d", got.DataBytes)
	}
	if got.UptimeSeconds != 86400 {
		t.Errorf("uptime = %d", got.UptimeSeconds)
	}
}

// MySQL and Redis use their own names for the same things, and the parser maps
// them rather than making each engine carry its own struct.
func TestOtherEnginesMapOntoTheSameFields(t *testing.T) {
	mysql := ParseEngineMetrics("mysql", strings.Join([]string{
		"Threads_connected=7",
		"Com_commit=900",
		"Com_rollback=1",
		"Uptime=3600",
		"max_connections=151",
	}, "\n"))
	if mysql.Connections != 7 || mysql.MaxConnections != 151 {
		t.Errorf("mysql connections = %d/%d, want 7/151", mysql.Connections, mysql.MaxConnections)
	}
	if mysql.Commits != 900 || mysql.UptimeSeconds != 3600 {
		t.Errorf("mysql commits/uptime = %d/%d", mysql.Commits, mysql.UptimeSeconds)
	}

	redis := ParseEngineMetrics("redis", strings.Join([]string{
		"connected_clients=12",
		"maxclients=10000",
		"uptime_in_seconds=7200",
		"used_memory=1048576",
		"keyspace_hits=990",
		"keyspace_misses=10",
	}, "\n"))
	if redis.Connections != 12 || redis.MaxConnections != 10000 {
		t.Errorf("redis connections = %d/%d", redis.Connections, redis.MaxConnections)
	}
	if redis.CacheHitRatio < 0.98 {
		t.Errorf("redis hit ratio = %f, want ~0.99", redis.CacheHitRatio)
	}
}

// An engine version that stops reporting something leaves a blank field rather
// than failing the whole reading. A metrics command that changed should not be
// the reason somebody cannot see the rest of the numbers.
func TestMissingAndUnknownKeysAreTolerated(t *testing.T) {
	got := ParseEngineMetrics("postgres", strings.Join([]string{
		"connections=5",
		"something_new_in_18=42",
		"not a pair at all",
		"",
	}, "\n"))

	if got.Connections != 5 {
		t.Errorf("connections = %d, want 5", got.Connections)
	}
	if got.MaxConnections != 0 || got.Commits != 0 {
		t.Errorf("absent fields were invented: %+v", got)
	}
	// The raw output is kept, because the question this struct does not answer
	// is answered by reading it.
	if !strings.Contains(got.Raw, "something_new_in_18") {
		t.Error("the raw output was discarded")
	}
}

// No numbers at all is a zero reading, not a divide by zero.
func TestAnEmptyReadingIsZeroRatherThanAPanic(t *testing.T) {
	got := ParseEngineMetrics("postgres", "")
	if got.CacheHitRatio != 0 {
		t.Errorf("cache hit ratio = %f on empty input", got.CacheHitRatio)
	}
	if got.Engine != "postgres" {
		t.Errorf("engine = %q", got.Engine)
	}
}
