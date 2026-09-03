import { defineConfig } from 'drizzle-kit';

// No 'driver' is set: drizzle-kit auto-selects the SQLite driver for
// migrate/push/studio from the runtime, picking the built-in node:sqlite on
// Node and bun:sqlite on Bun (it only reaches for better-sqlite3 when that
// package is present, which this app does not install). That auto-selection is
// what keeps `webjs db migrate` free of a native driver, matching the runtime
// connection in db/connection.server.ts.
export default defineConfig({
  dialect: 'sqlite',
  schema: './db/schema.server.ts',
  out: './db/migrations',
  dbCredentials: { url: process.env.DATABASE_URL?.replace(/^file:/, '') ?? 'db/dev.db' },
});
