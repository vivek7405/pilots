/**
 * What a database engine says about itself.
 *
 * The mirror of `sdks/go/dbmetrics.go`. It is a mirror rather than a route
 * because the reading is an EXEC: every engine has its own client inside its
 * own image, so asking it costs nothing to build and nothing to keep running,
 * where an exporter per database would have to be installed, supervised and
 * kept in step with four engines' versions.
 *
 * `test/dbmetrics.test.ts` reads the Go file and fails when the two sides stop
 * agreeing about which keys exist, which is the drift that actually happens: a
 * metric added on one side and not the other.
 */

/** The engines a recipe exists for, which is exactly what this reads. */
export const METRIC_ENGINES = ['postgres', 'mysql', 'redis', 'mongo'] as const
export type MetricEngine = (typeof METRIC_ENGINES)[number]

/**
 * The same seven questions answered for every engine.
 *
 * Not every engine answers all of them. Redis has no rollbacks and Mongo no
 * cache hit ratio in this sense, and an unanswered one stays zero rather than
 * being invented.
 */
export interface EngineMetrics {
  engine: string
  /** In use, and the ceiling. Either number alone says nothing. */
  connections: number
  max_connections: number
  /** Reads served from memory, 0 to 1. */
  cache_hit_ratio: number
  commits: number
  rollbacks: number
  data_bytes: number
  /** The ENGINE's uptime, not the machine's. */
  uptime_seconds: number
  /** The engine's own output, unparsed. */
  raw: string
}

/**
 * What to run inside a machine to get one engine's figures.
 *
 * One line per engine, emitting `key=value` pairs, so the parser below is the
 * same for all four. Four output formats would be four parsers, and three of
 * them would be wrong the first time an engine changed a column.
 */
export function metricsCommand(engine: string): string {
  switch (engine) {
    case 'postgres':
      return (
        `psql -U postgres -Atq -F= -c "SELECT 'connections', count(*) FROM pg_stat_activity` +
        ` UNION ALL SELECT 'max_connections', setting::int FROM pg_settings WHERE name='max_connections'` +
        ` UNION ALL SELECT 'commits', sum(xact_commit)::bigint FROM pg_stat_database` +
        ` UNION ALL SELECT 'rollbacks', sum(xact_rollback)::bigint FROM pg_stat_database` +
        ` UNION ALL SELECT 'blks_hit', sum(blks_hit)::bigint FROM pg_stat_database` +
        ` UNION ALL SELECT 'blks_read', sum(blks_read)::bigint FROM pg_stat_database` +
        ` UNION ALL SELECT 'data_bytes', sum(pg_database_size(datname))::bigint FROM pg_database` +
        ` UNION ALL SELECT 'uptime', extract(epoch from now()-pg_postmaster_start_time())::bigint"`
      )
    case 'mysql':
      return (
        `mysql -N -B -e "SHOW GLOBAL STATUS WHERE Variable_name IN ` +
        `('Threads_connected','Com_commit','Com_rollback','Uptime'); ` +
        `SHOW VARIABLES WHERE Variable_name='max_connections'" | tr '\\t' '='`
      )
    case 'redis':
      return (
        `redis-cli -a "$REDIS_PASSWORD" --no-auth-warning INFO | tr -d '\\r' | grep -E ` +
        `'^(connected_clients|maxclients|uptime_in_seconds|used_memory|keyspace_hits|keyspace_misses):' | tr ':' '='`
      )
    case 'mongo':
      return (
        `mongosh --quiet --eval 'const s=db.serverStatus(); ` +
        `print("connections="+s.connections.current); ` +
        `print("max_connections="+(s.connections.current+s.connections.available)); ` +
        `print("uptime="+s.uptime); ` +
        `print("data_bytes="+db.stats().dataSize)'`
      )
    default:
      return ''
  }
}

/**
 * Reads the `key=value` output of `metricsCommand`.
 *
 * Unknown keys are ignored and missing ones stay zero, so an engine version
 * that stops reporting something degrades to a blank field rather than to an
 * error. A metrics command that half fails should not be the reason somebody
 * cannot see the rest of the numbers.
 */
export function parseEngineMetrics(engine: string, raw: string): EngineMetrics {
  const out: EngineMetrics = {
    engine,
    connections: 0,
    max_connections: 0,
    cache_hit_ratio: 0,
    commits: 0,
    rollbacks: 0,
    data_bytes: 0,
    uptime_seconds: 0,
    raw,
  }
  let hits = 0
  let reads = 0
  let misses = 0

  for (const line of raw.split('\n')) {
    const at = line.trim().indexOf('=')
    if (at < 0) continue
    const key = line.trim().slice(0, at).trim().toLowerCase()
    const value = line.trim().slice(at + 1).trim()

    switch (key) {
      case 'connections':
      case 'threads_connected':
      case 'connected_clients':
        out.connections = num(value)
        break
      case 'max_connections':
      case 'maxclients':
        out.max_connections = num(value)
        break
      case 'commits':
      case 'com_commit':
        out.commits = num(value)
        break
      case 'rollbacks':
      case 'com_rollback':
        out.rollbacks = num(value)
        break
      case 'uptime':
      case 'uptime_in_seconds':
        out.uptime_seconds = num(value)
        break
      case 'data_bytes':
      case 'used_memory':
        out.data_bytes = num(value)
        break
      case 'blks_hit':
      case 'keyspace_hits':
        hits = num(value)
        break
      case 'blks_read':
        reads = num(value)
        break
      case 'keyspace_misses':
        misses = num(value)
        break
    }
  }

  // Computed rather than reported, because no engine reports it directly and
  // two numbers with a division between them is one fewer thing to keep in step
  // across four engines.
  const total = hits + reads + misses
  if (total > 0) out.cache_hit_ratio = hits / total
  return out
}

/** A number, or zero. A field nothing parsed must not become NaN on a screen. */
function num(raw: string): number {
  const parsed = Number(raw)
  return Number.isFinite(parsed) ? parsed : 0
}
