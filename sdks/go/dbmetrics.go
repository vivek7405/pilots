package pilots

import (
	"strconv"
	"strings"
)

// What a database engine says about itself.
//
// # Why this is an exec and not a scrape
//
// The platform's own metrics are about the MACHINE: CPU, memory, requests,
// restores. None of them can answer "is this database about to run out of
// connections", because that number exists only inside the engine.
//
// Reading it means asking the engine, and every engine has its own client
// already inside its own image: psql, mysql, redis-cli, mongosh. So the reading
// is an exec, which costs nothing to build and nothing to keep running, rather
// than an exporter process per database that has to be installed, supervised
// and kept in step with four engines' versions.
//
// # Why so few numbers
//
// These seven are the ones that change a decision. Connections against the
// limit says whether to add a pooler. Cache hit ratio says whether the machine
// is too small. Rollbacks against commits says whether something is failing
// quietly. Everything else an engine reports is worth reading when you already
// know which question you are asking, and `--json` hands over the raw output
// for exactly that.

// EngineMetrics is the same seven questions answered for every engine.
//
// Not every engine answers all of them -- Redis has no rollbacks, Mongo no
// cache hit ratio in this sense -- and an unanswered one stays zero rather
// than being invented.
type EngineMetrics struct {
	Engine string `json:"engine"`
	// Connections in use, and the ceiling. The pair is the point: either number
	// alone says nothing about how close the database is to refusing work.
	Connections    int `json:"connections"`
	MaxConnections int `json:"max_connections"`
	// CacheHitRatio is reads served from memory, 0 to 1. Below about 0.99 on a
	// busy Postgres means the working set no longer fits, which is a machine
	// size problem rather than a query problem.
	CacheHitRatio float64 `json:"cache_hit_ratio"`
	// Commits and Rollbacks since the engine started. A rollback rate that
	// climbs without a deploy is an application failing quietly.
	Commits   int64 `json:"commits"`
	Rollbacks int64 `json:"rollbacks"`
	// DataBytes is what the data occupies, as the engine counts it. Compared
	// against the volume's size, this is how far off running out you are.
	DataBytes int64 `json:"data_bytes"`
	// UptimeSeconds since the engine started, which is NOT the machine's
	// uptime: a database that restarted an hour ago inside a machine that has
	// been up for a week is a database that crashed.
	UptimeSeconds int64 `json:"uptime_seconds"`
	// Raw is the engine's own output, unparsed, for the question this struct
	// does not answer.
	Raw string `json:"raw,omitempty"`
}

// MetricsCommand is what to run inside a machine to get one engine's figures.
//
// One line per engine, emitting `key=value` pairs, so the parser below is the
// same for all four. Shaped that way deliberately: four output formats would be
// four parsers, and three of them would be wrong the first time an engine
// changed its display.
func MetricsCommand(engine string) string {
	switch engine {
	case "postgres":
		// One query, one row, named columns. pg_stat_database sums across
		// databases because a pilots Postgres holds one that matters and the
		// template databases are noise.
		return `psql -U postgres -Atq -F= -c "SELECT 'connections', count(*) FROM pg_stat_activity` +
			` UNION ALL SELECT 'max_connections', setting::int FROM pg_settings WHERE name='max_connections'` +
			` UNION ALL SELECT 'commits', sum(xact_commit)::bigint FROM pg_stat_database` +
			` UNION ALL SELECT 'rollbacks', sum(xact_rollback)::bigint FROM pg_stat_database` +
			` UNION ALL SELECT 'blks_hit', sum(blks_hit)::bigint FROM pg_stat_database` +
			` UNION ALL SELECT 'blks_read', sum(blks_read)::bigint FROM pg_stat_database` +
			` UNION ALL SELECT 'data_bytes', sum(pg_database_size(datname))::bigint FROM pg_database` +
			` UNION ALL SELECT 'uptime', extract(epoch from now()-pg_postmaster_start_time())::bigint"`
	case "mysql":
		return `mysql -N -B -e "SHOW GLOBAL STATUS WHERE Variable_name IN ` +
			`('Threads_connected','Com_commit','Com_rollback','Uptime'); ` +
			`SHOW VARIABLES WHERE Variable_name='max_connections'" | tr '\t' '='`
	case "redis":
		return `redis-cli -a "$REDIS_PASSWORD" --no-auth-warning INFO | tr -d '\r' | grep -E ` +
			`'^(connected_clients|maxclients|uptime_in_seconds|used_memory|keyspace_hits|keyspace_misses):' | tr ':' '='`
	case "mongo":
		return `mongosh --quiet --eval 'const s=db.serverStatus(); ` +
			`print("connections="+s.connections.current); ` +
			`print("max_connections="+(s.connections.current+s.connections.available)); ` +
			`print("uptime="+s.uptime); ` +
			`print("data_bytes="+db.stats().dataSize)'`
	default:
		return ""
	}
}

// ParseEngineMetrics reads the key=value output of MetricsCommand.
//
// Unknown keys are ignored and missing ones stay zero, so an engine version
// that stops reporting something degrades to a blank field rather than to an
// error. A metrics command that fails should not be the reason somebody cannot
// see the rest of the numbers.
func ParseEngineMetrics(engine, raw string) EngineMetrics {
	m := EngineMetrics{Engine: engine, Raw: raw}
	var hits, reads, misses float64

	for _, line := range strings.Split(raw, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)

		switch key {
		case "connections", "threads_connected", "connected_clients":
			m.Connections = atoi(value)
		case "max_connections", "maxclients":
			m.MaxConnections = atoi(value)
		case "commits", "com_commit":
			m.Commits = atoi64(value)
		case "rollbacks", "com_rollback":
			m.Rollbacks = atoi64(value)
		case "uptime", "uptime_in_seconds":
			m.UptimeSeconds = atoi64(value)
		case "data_bytes", "used_memory":
			m.DataBytes = atoi64(value)
		case "blks_hit", "keyspace_hits":
			hits = atof(value)
		case "blks_read":
			reads = atof(value)
		case "keyspace_misses":
			misses = atof(value)
		}
	}

	// Computed rather than reported, because no engine reports it directly and
	// two numbers with a division between them is one fewer thing to keep in
	// step across four engines.
	if total := hits + reads + misses; total > 0 {
		m.CacheHitRatio = hits / total
	}
	return m
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func atoi64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func atof(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}
