/**
 * Running one query against one database, through a tunnel.
 *
 * # Read-only by default, and what that actually means
 *
 * A data browser that can drop a table by accident is worse than no data
 * browser, so every engine here is put into the strongest read-only mode it
 * offers before the query runs, and lifting it is a separate deliberate act
 * with its own confirmation in the UI.
 *
 * The enforcement is the ENGINE's, never a regular expression over the query
 * text. A statement filter is the wrong shape: it is a denylist, it has to
 * understand comments, dollar quoting and multi-statement bodies, and the first
 * thing it misses is the one that drops the table. Postgres and MySQL both have
 * a real read-only transaction; Redis and Mongo have no such mode, so those two
 * are an allowlist of read COMMANDS, which is a closed set rather than an open
 * grammar.
 *
 * # Why the drivers and not the engine's own client over exec
 *
 * Typed values and paging. `psql --csv` through exec would work and would have
 * added no dependency, but everything comes back as text: a NULL and the string
 * "NULL" are the same four characters, a bigint loses precision on the way
 * through, and paging becomes string surgery. A data browser that cannot tell
 * those apart is one that shows the wrong thing confidently.
 */

import type { Tunnel } from '#modules/data/tunnel.server.ts';

/** The engines a recipe exists for, which is exactly what this supports. */
export const ENGINES = ['postgres', 'mysql', 'redis', 'mongo'] as const;
export type Engine = (typeof ENGINES)[number];

export function isEngine(value: unknown): value is Engine {
  return (ENGINES as readonly string[]).includes(String(value));
}

/** Where each engine listens inside its machine. */
export const ENGINE_PORT: Record<Engine, number> = {
  postgres: 5432,
  mysql: 3306,
  redis: 6379,
  mongo: 27017,
};

export interface QueryRequest {
  engine: Engine;
  tunnel: Tunnel;
  /** The connection string `pilot add` generated, with the host rewritten. */
  url: string;
  query: string;
  /** The collection or key space a Mongo query runs in. */
  target?: string;
  /** Lifts the read-only mode. The caller has already confirmed. */
  write?: boolean;
  limit?: number;
}

export interface QueryResult {
  columns: string[];
  rows: unknown[][];
  /** How many rows a write touched, when the engine reports it. */
  affected?: number;
  /** True when the engine refused to go further than `limit`. */
  truncated?: boolean;
  /** Set when the query ran with the read-only mode lifted. */
  wrote?: boolean;
}

const DEFAULT_LIMIT = 200;

/**
 * Rewrites a stored connection string to point at the tunnel.
 *
 * The credentials, database name and options are the ones `pilot add`
 * generated. Only the authority moves, because everything else in that string
 * is part of what makes the connection work and rebuilding it here would be a
 * second place the format is written down.
 */
export function throughTunnel(url: string, tunnel: Tunnel): string {
  const parsed = new URL(url);
  parsed.hostname = tunnel.host;
  parsed.port = String(tunnel.port);
  return parsed.toString();
}

export async function runQuery(req: QueryRequest): Promise<QueryResult> {
  const limit = req.limit && req.limit > 0 ? Math.min(req.limit, 1000) : DEFAULT_LIMIT;
  switch (req.engine) {
    case 'postgres':
      return runPostgres(req, limit);
    case 'mysql':
      return runMySQL(req, limit);
    case 'redis':
      return runRedis(req, limit);
    case 'mongo':
      return runMongo(req, limit);
  }
}

async function runPostgres(req: QueryRequest, limit: number): Promise<QueryResult> {
  const { Client } = await import('pg');
  const client = new Client({ connectionString: throughTunnel(req.url, req.tunnel) });
  await client.connect();
  try {
    // A transaction either way, so a read is a consistent snapshot and a write
    // is one unit that either lands or does not. READ ONLY is the engine's own
    // enforcement: it refuses INSERT, UPDATE, DELETE, every DDL and every
    // function that writes, which no filter over the query text could match.
    await client.query(req.write ? 'BEGIN' : 'BEGIN READ ONLY');
    try {
      const res = await client.query({ text: req.query, rowMode: 'array' });
      await client.query('COMMIT');
      const fields = res.fields ?? [];
      const rows = (res.rows as unknown[][]) ?? [];
      return {
        columns: fields.map((f: { name: string }) => f.name),
        rows: rows.slice(0, limit),
        affected: typeof res.rowCount === 'number' ? res.rowCount : undefined,
        truncated: rows.length > limit,
        wrote: req.write === true,
      };
    } catch (err) {
      await client.query('ROLLBACK').catch(() => {});
      throw err;
    }
  } finally {
    await client.end();
  }
}

async function runMySQL(req: QueryRequest, limit: number): Promise<QueryResult> {
  const mysql = await import('mysql2/promise');
  const conn = await mysql.createConnection(throughTunnel(req.url, req.tunnel));
  try {
    if (!req.write) {
      // MySQL's session-level equivalent. The next transaction is read only,
      // and the engine refuses a write inside it.
      await conn.query('SET SESSION TRANSACTION READ ONLY');
    }
    await conn.beginTransaction();
    try {
      const [rows, fields] = await conn.query({ sql: req.query, rowsAsArray: true });
      await conn.commit();
      if (Array.isArray(rows)) {
        const all = rows as unknown[][];
        return {
          columns: (fields ?? []).map((f: { name: string }) => f.name),
          rows: all.slice(0, limit),
          truncated: all.length > limit,
          wrote: req.write === true,
        };
      }
      // A write answers with a result header rather than rows.
      const header = rows as { affectedRows?: number };
      return {
        columns: [],
        rows: [],
        affected: header.affectedRows,
        wrote: req.write === true,
      };
    } catch (err) {
      await conn.rollback().catch(() => {});
      throw err;
    }
  } finally {
    await conn.end();
  }
}

/**
 * Redis commands that only read.
 *
 * An ALLOWLIST rather than a denylist of writes, because the set of commands
 * that can destroy data is open -- it grows with every Redis release and with
 * every module somebody loads -- while the set worth browsing with is small and
 * closed. A command not on this list is refused by name, which is a better
 * error than one that ran.
 */
const REDIS_READS = new Set([
  'get', 'mget', 'strlen', 'exists', 'ttl', 'pttl', 'type', 'randomkey',
  'keys', 'scan', 'dbsize',
  'hget', 'hmget', 'hgetall', 'hkeys', 'hvals', 'hlen', 'hexists', 'hscan',
  'lrange', 'llen', 'lindex',
  'smembers', 'scard', 'sismember', 'sscan', 'srandmember',
  'zrange', 'zrevrange', 'zrangebyscore', 'zcard', 'zscore', 'zscan', 'zcount',
  'getrange', 'bitcount', 'object', 'memory', 'info', 'ping', 'time', 'lastsave',
]);

async function runRedis(req: QueryRequest, limit: number): Promise<QueryResult> {
  const parts = splitCommand(req.query);
  if (parts.length === 0) throw new Error('no command');
  const name = parts[0]!.toLowerCase();
  if (!req.write && !REDIS_READS.has(name)) {
    throw new Error(
      `${parts[0]} is not a read command. Redis has no read-only mode, so this is an ` +
        'allowlist; turn on Write to run it.',
    );
  }

  const { Redis } = await import('ioredis');
  const url = new URL(throughTunnel(req.url, req.tunnel));
  const redis = new Redis({
    host: url.hostname,
    port: Number(url.port),
    password: decodeURIComponent(url.password),
    // One attempt. A browser query that silently retried against a database
    // somebody is watching would run twice with nothing saying so.
    maxRetriesPerRequest: 0,
    retryStrategy: () => null,
  });
  try {
    const reply = await redis.call(name, ...parts.slice(1));
    const rows = Array.isArray(reply)
      ? reply.map((value, i) => [i, stringify(value)])
      : [[0, stringify(reply)]];
    return {
      columns: Array.isArray(reply) ? ['#', 'value'] : ['', 'value'],
      rows: rows.slice(0, limit),
      truncated: rows.length > limit,
      wrote: req.write === true,
    };
  } finally {
    redis.disconnect();
  }
}

/**
 * Mongo operations that only read.
 *
 * Same reasoning as Redis: no read-only session mode, so a closed set of
 * operations rather than an open grammar. `find` and `aggregate` are what a
 * browser needs; an aggregate carrying a `$out` or `$merge` stage writes, so
 * those two stages are refused by name.
 */
const MONGO_READS = new Set(['find', 'aggregate', 'count', 'distinct', 'listCollections']);
const MONGO_WRITING_STAGES = new Set(['$out', '$merge']);

async function runMongo(req: QueryRequest, limit: number): Promise<QueryResult> {
  const parsed: { op?: string; filter?: unknown; pipeline?: Record<string, unknown>[] } = req.query.trim()
    ? JSON.parse(req.query)
    : { op: 'find', filter: {} };
  const op = parsed.op ?? 'find';
  if (!req.write && !MONGO_READS.has(op)) {
    throw new Error(
      `${op} is not a read operation. Mongo has no read-only session, so this is an ` +
        'allowlist; turn on Write to run it.',
    );
  }
  if (op === 'aggregate') {
    for (const stage of parsed.pipeline ?? []) {
      for (const key of Object.keys(stage)) {
        if (MONGO_WRITING_STAGES.has(key)) {
          throw new Error(`${key} writes a collection; it is not a read stage`);
        }
      }
    }
  }
  if (!req.target) throw new Error('name a collection');

  const { MongoClient } = await import('mongodb');
  const client = new MongoClient(throughTunnel(req.url, req.tunnel), {
    directConnection: true,
    serverSelectionTimeoutMS: 5000,
  });
  try {
    await client.connect();
    const collection = client.db().collection(req.target);
    const docs =
      op === 'aggregate'
        ? await collection.aggregate(parsed.pipeline ?? []).limit(limit + 1).toArray()
        : await collection.find((parsed.filter ?? {}) as object).limit(limit + 1).toArray();
    // Columns are the union of every key seen, in first-seen order, because a
    // document store has no schema and taking the first document's keys as the
    // header silently hides every field the rest of them have.
    const columns: string[] = [];
    for (const doc of docs) {
      for (const key of Object.keys(doc)) if (!columns.includes(key)) columns.push(key);
    }
    return {
      columns,
      rows: docs.slice(0, limit).map((doc) => columns.map((key) => stringify((doc as Record<string, unknown>)[key]))),
      truncated: docs.length > limit,
      wrote: req.write === true,
    };
  } finally {
    await client.close();
  }
}

/**
 * Splits a Redis command line, honouring quotes.
 *
 * Needed because a value can contain spaces, and splitting on whitespace alone
 * turns `SET k "a b"` into a command with three arguments.
 */
export function splitCommand(line: string): string[] {
  const out: string[] = [];
  let current = '';
  let quote: string | null = null;
  let started = false;
  for (const ch of line.trim()) {
    if (quote) {
      if (ch === quote) quote = null;
      else current += ch;
      continue;
    }
    if (ch === '"' || ch === "'") {
      quote = ch;
      started = true;
      continue;
    }
    if (/\s/.test(ch)) {
      if (current || started) out.push(current);
      current = '';
      started = false;
      continue;
    }
    current += ch;
    started = true;
  }
  if (current || started) out.push(current);
  return out;
}

/** Renders one cell without losing the difference between null and "null". */
function stringify(value: unknown): string {
  if (value === null) return '∅';
  if (value === undefined) return '';
  if (typeof value === 'string') return value;
  if (value instanceof Date) return value.toISOString();
  if (Buffer.isBuffer(value)) return value.toString('base64');
  return JSON.stringify(value);
}
