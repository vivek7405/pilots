/**
 * The engine-metrics mirror, held against the Go original.
 *
 * `sdks/go/dbmetrics.go` is the source this was copied from, and a copy that
 * nothing compares is a copy that drifts. The realistic drift is not a reworded
 * comment: it is a metric key added on one side and not the other, which
 * produces a dashboard quietly missing a number the CLI shows. So the assertion
 * is over the KEYS, read out of the Go file itself.
 */

import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

import { metricsCommand, parseEngineMetrics, METRIC_ENGINES } from '../src/dbmetrics.ts'

const GO = readFileSync(fileURLToPath(new URL('../../go/dbmetrics.go', import.meta.url)), 'utf8')

/** Every `case "..."` label inside ParseEngineMetrics, which is its key set. */
function goParserKeys(): string[] {
  const body = GO.slice(GO.indexOf('func ParseEngineMetrics'))
  const end = body.indexOf('\nfunc ')
  const parser = end < 0 ? body : body.slice(0, end)
  const keys: string[] = []
  for (const match of parser.matchAll(/case ((?:"[a-z_]+"(?:, )?)+):/g)) {
    for (const key of match[1]!.matchAll(/"([a-z_]+)"/g)) keys.push(key[1]!)
  }
  return keys
}

/** The same set, read out of the TypeScript parser. */
function tsParserKeys(): string[] {
  const src = readFileSync(fileURLToPath(new URL('../src/dbmetrics.ts', import.meta.url)), 'utf8')
  const body = src.slice(src.indexOf('export function parseEngineMetrics'))
  return [...body.matchAll(/case '([a-z_]+)':/g)].map((m) => m[1]!)
}

test('both parsers know exactly the same metric keys', () => {
  const go = goParserKeys().sort()
  const ts = tsParserKeys().sort()
  assert.ok(go.length > 0, 'read no keys out of the Go parser; the scanner is broken, not the code')
  assert.deepEqual(
    ts,
    go,
    'the two parsers disagree about which keys exist, so one of them silently ' +
      'drops a number the other shows',
  )
})

test('every engine has a command, and it is the Go one', () => {
  for (const engine of METRIC_ENGINES) {
    const cmd = metricsCommand(engine)
    assert.ok(cmd !== '', `${engine} has no metrics command`)
    // The distinctive fragment of each engine's command, which is what would
    // change if somebody rewrote one side's query and not the other's.
    const fragment = {
      postgres: 'pg_stat_activity',
      mysql: 'SHOW GLOBAL STATUS',
      redis: 'redis-cli',
      mongo: 'db.serverStatus()',
    }[engine]
    assert.ok(cmd.includes(fragment), `${engine}: ${cmd}`)
    assert.ok(GO.includes(fragment), `the Go command no longer contains ${fragment}`)
  }
})

test('an unknown engine has no command rather than a wrong one', () => {
  assert.equal(metricsCommand('cockroach'), '')
})

test('postgres output becomes the numbers a person acts on', () => {
  const got = parseEngineMetrics(
    'postgres',
    [
      'connections=42',
      'max_connections=100',
      'commits=900',
      'rollbacks=100',
      'blks_hit=990',
      'blks_read=10',
      'data_bytes=1048576',
      'uptime=3600',
    ].join('\n'),
  )
  assert.equal(got.connections, 42)
  assert.equal(got.max_connections, 100)
  assert.equal(got.commits, 900)
  assert.equal(got.rollbacks, 100)
  assert.equal(got.data_bytes, 1048576)
  assert.equal(got.uptime_seconds, 3600)
  assert.equal(got.cache_hit_ratio, 0.99)
})

// Redis reports hits and misses rather than hits and reads, and the ratio has
// to come out the same way round. Inverted, a healthy cache reads as a broken
// one, which is the kind of wrong number that gets a machine resized that was
// fine.
test('redis hits and misses give the same ratio as postgres hits and reads', () => {
  const got = parseEngineMetrics(
    'redis',
    [
      'connected_clients=5',
      'maxclients=10000',
      'keyspace_hits=990',
      'keyspace_misses=10',
      'used_memory=2048',
      'uptime_in_seconds=60',
    ].join('\n'),
  )
  assert.equal(got.connections, 5)
  assert.equal(got.max_connections, 10000)
  assert.equal(got.cache_hit_ratio, 0.99)
  assert.equal(got.data_bytes, 2048)
  assert.equal(got.uptime_seconds, 60)
  // Redis has no transactions to report, and an unanswered question stays zero
  // rather than being invented.
  assert.equal(got.commits, 0)
  assert.equal(got.rollbacks, 0)
})

// An engine version that stops reporting something must degrade to a blank
// field, never to an error or to NaN on a screen.
test('unknown keys are ignored and unparsable values become zero', () => {
  const got = parseEngineMetrics(
    'mysql',
    'Threads_connected=7\nSomething_New=1\nCom_commit=nonsense\ngarbage',
  )
  assert.equal(got.connections, 7)
  assert.equal(got.commits, 0)
  assert.equal(got.cache_hit_ratio, 0)
})
