import { isAbsolute, resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import * as schema from './schema.server.ts';

// The only file that opens the driver. Runtime-neutral and ZERO native deps:
// built-in bun:sqlite on Bun, built-in node:sqlite on Node. Cached on
// globalThis across dev reloads.
// A relative SQLite path resolves against the app root (the parent of db/), not
// process.cwd(), so the connection works under `webjs dev` AND when the app is
// embedded via createRequestHandler from a different working directory.
const appRoot = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const raw = process.env.DATABASE_URL?.replace(/^file:/, '') ?? 'db/dev.db';
const url = raw === ':memory:' || isAbsolute(raw) ? raw : resolve(appRoot, raw);
const g = globalThis as unknown as { __webjs_db?: unknown };

// Both node:sqlite and bun:sqlite default `busy_timeout` to 0, so a concurrent
// writer throws `database is locked` immediately. Restore a 5s wait (the old
// better-sqlite3 default) so contended access waits, and WAL so readers proceed
// alongside one writer.
function tune<T extends { exec(sql: string): unknown }>(client: T): T {
  client.exec('PRAGMA busy_timeout = 5000');
  client.exec('PRAGMA journal_mode = WAL');
  return client;
}

async function open() {
  if ((globalThis as { Bun?: unknown }).Bun) {
    // @ts-expect-error bun:sqlite is a Bun builtin with no Node typings
    const { Database } = await import('bun:sqlite');
    const { drizzle } = await import('drizzle-orm/bun-sqlite');
    return drizzle({ client: tune(new Database(url)), relations: schema.relations });
  }
  const { DatabaseSync } = await import('node:sqlite');
  const { drizzle } = await import('drizzle-orm/node-sqlite');
  return drizzle({ client: tune(new DatabaseSync(url)), relations: schema.relations });
}

export const db = (g.__webjs_db ??= await open()) as Awaited<ReturnType<typeof open>>;
